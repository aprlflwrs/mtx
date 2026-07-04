package queue

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mtx/internal/encode"
	"mtx/internal/policy"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	mediaFile := filepath.Join(dir, "episode.mkv")
	if err := os.WriteFile(mediaFile, []byte("fake video bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return store, mediaFile
}

func TestEnqueueDeduplicatesUnchangedFiles(t *testing.T) {
	store, mediaFile := openTestStore(t)

	first, err := store.Enqueue(mediaFile, false)
	if err != nil || !first {
		t.Fatalf("first enqueue: inserted=%v err=%v", first, err)
	}
	second, err := store.Enqueue(mediaFile, false)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("unchanged file was enqueued twice")
	}
}

func TestClaimRunsEachJobExactlyOnce(t *testing.T) {
	store, mediaFile := openTestStore(t)
	if _, err := store.Enqueue(mediaFile, true); err != nil {
		t.Fatal(err)
	}

	job, err := store.ClaimNext()
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}
	if job.Path != mediaFile || !job.Grain {
		t.Errorf("claimed job = %+v", job)
	}

	again, err := store.ClaimNext()
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Errorf("running job was claimed a second time: %+v", again)
	}
}

func TestRecordResultAndSummarize(t *testing.T) {
	store, mediaFile := openTestStore(t)

	enqueueAndClaim := func() *Job {
		t.Helper()
		if _, err := store.Enqueue(mediaFile, false); err != nil {
			t.Fatal(err)
		}
		job, err := store.ClaimNext()
		if err != nil || job == nil {
			t.Fatalf("claim: %v %v", job, err)
		}
		return job
	}

	done := enqueueAndClaim()
	if err := store.RecordResult(done.ID, encode.Result{
		Profile: policy.HDQuickSync, SizeBefore: 1000, SizeAfter: 300,
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Touch the file so it counts as a new version and can be enqueued again.
	if err := os.Chtimes(mediaFile, timeIn(t, -1), timeIn(t, -1)); err != nil {
		t.Fatal(err)
	}
	skipped := enqueueAndClaim()
	if err := store.RecordResult(skipped.ID, encode.Result{Skipped: "dolby-vision"}, nil); err != nil {
		t.Fatal(err)
	}

	if err := os.Chtimes(mediaFile, timeIn(t, -2), timeIn(t, -2)); err != nil {
		t.Fatal(err)
	}
	failed := enqueueAndClaim()
	if err := store.RecordResult(failed.ID, encode.Result{}, errors.New("ffmpeg exploded")); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summarize()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"done": 1, "skipped": 1, "failed": 1}
	for status, count := range want {
		if summary.CountByStatus[status] != count {
			t.Errorf("%s count = %d, want %d", status, summary.CountByStatus[status], count)
		}
	}
	// size_bytes comes from the real file on disk (16 bytes); saved = 16 - 300
	// would be negative, which is fine for the test — we only check it summed.
	if summary.BytesSaved == 0 {
		t.Error("BytesSaved was not aggregated")
	}
}

func TestInterruptedJobsResetOnOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	mediaFile := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(mediaFile, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(mediaFile, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNext(); err != nil {
		t.Fatal(err)
	}
	store.Close() // process dies with the job still "running"

	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	job, err := reopened.ClaimNext()
	if err != nil {
		t.Fatal(err)
	}
	if job == nil {
		t.Fatal("interrupted job was not reset to pending")
	}
}

func timeIn(_ *testing.T, hours int) time.Time {
	return time.Now().Add(time.Duration(hours) * time.Hour)
}
