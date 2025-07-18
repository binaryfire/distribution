package storage

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/internal/uuid"
	"github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/inmemory"
)

func testUploadFS(t *testing.T, numUploads int, repoName string, startedAt time.Time) (driver.StorageDriver, context.Context) {
	d := inmemory.New()
	ctx := context.Background()
	for i := 0; i < numUploads; i++ {
		addUploads(ctx, t, d, uuid.NewString(), repoName, startedAt)
	}
	return d, ctx
}

func addUploads(ctx context.Context, t *testing.T, d driver.StorageDriver, uploadID, repo string, startedAt time.Time) {
	dataPath, err := pathFor(uploadDataPathSpec{name: repo, id: uploadID})
	if err != nil {
		t.Fatal("Unable to resolve path")
	}
	if err := d.PutContent(ctx, dataPath, []byte("")); err != nil {
		t.Fatal("Unable to write data file")
	}

	startedAtPath, err := pathFor(uploadStartedAtPathSpec{name: repo, id: uploadID})
	if err != nil {
		t.Fatal("Unable to resolve path")
	}

	if err := d.PutContent(ctx, startedAtPath, []byte(startedAt.Format(time.RFC3339))); err != nil {
		t.Fatal("Unable to write startedAt file")
	}
}

func TestPurgeGather(t *testing.T) {
	uploadCount := 5
	fs, ctx := testUploadFS(t, uploadCount, "test-repo", time.Now())
	uploadData, errs := getOutstandingUploads(ctx, fs)
	if len(errs) != 0 {
		t.Errorf("Unexpected errors: %q", errs)
	}
	if len(uploadData) != uploadCount {
		t.Errorf("Unexpected upload file count: %d != %d", uploadCount, len(uploadData))
	}
}

func TestPurgeNone(t *testing.T) {
	fs, ctx := testUploadFS(t, 10, "test-repo", time.Now())
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	deleted, errs := PurgeUploads(ctx, fs, oneHourAgo, true)
	if len(errs) != 0 {
		t.Error("Unexpected errors", errs)
	}
	if len(deleted) != 0 {
		t.Errorf("Unexpectedly deleted files for time: %s", oneHourAgo)
	}
}

func TestPurgeAll(t *testing.T) {
	uploadCount := 10
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	fs, ctx := testUploadFS(t, uploadCount, "test-repo", oneHourAgo)

	// Ensure > 1 repos are purged
	addUploads(ctx, t, fs, uuid.NewString(), "test-repo2", oneHourAgo)
	uploadCount++

	deleted, errs := PurgeUploads(ctx, fs, time.Now(), true)
	if len(errs) != 0 {
		t.Error("Unexpected errors:", errs)
	}
	fileCount := uploadCount
	if len(deleted) != fileCount {
		t.Errorf("Unexpectedly deleted file count %d != %d",
			len(deleted), fileCount)
	}
}

func TestPurgeSome(t *testing.T) {
	oldUploadCount := 5
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	fs, ctx := testUploadFS(t, oldUploadCount, "library/test-repo", oneHourAgo)

	newUploadCount := 4

	for i := 0; i < newUploadCount; i++ {
		addUploads(ctx, t, fs, uuid.NewString(), "test-repo", time.Now().Add(1*time.Hour))
	}

	deleted, errs := PurgeUploads(ctx, fs, time.Now(), true)
	if len(errs) != 0 {
		t.Error("Unexpected errors:", errs)
	}
	if len(deleted) != oldUploadCount {
		t.Errorf("Unexpectedly deleted file count %d != %d",
			len(deleted), oldUploadCount)
	}
}

func TestPurgeOnlyUploads(t *testing.T) {
	oldUploadCount := 5
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	fs, ctx := testUploadFS(t, oldUploadCount, "test-repo", oneHourAgo)

	// Create a directory tree outside _uploads and ensure
	// these files aren't deleted.
	dataPath, err := pathFor(uploadDataPathSpec{name: "test-repo", id: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	nonUploadPath := strings.Replace(dataPath, "_upload", "_important", -1)
	if strings.Contains(nonUploadPath, "_upload") {
		t.Fatal("Non-upload path not created correctly")
	}

	nonUploadFile := path.Join(nonUploadPath, "file")
	if err = fs.PutContent(ctx, nonUploadFile, []byte("")); err != nil {
		t.Fatal("Unable to write data file")
	}

	deleted, errs := PurgeUploads(ctx, fs, time.Now(), true)
	if len(errs) != 0 {
		t.Error("Unexpected errors", errs)
	}
	for _, file := range deleted {
		if !strings.Contains(file, "_upload") {
			t.Error("Non-upload file deleted")
		}
	}
}

func TestPurgeMissingStartedAt(t *testing.T) {
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	fs, ctx := testUploadFS(t, 1, "test-repo", oneHourAgo)

	err := fs.Walk(ctx, "/", func(fileInfo driver.FileInfo) error {
		filePath := fileInfo.Path()
		_, file := path.Split(filePath)

		if file == "startedat" {
			if err := fs.Delete(ctx, filePath); err != nil {
				t.Fatalf("Unable to delete startedat file: %s", filePath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Unexpected error during Walk: %s ", err.Error())
	}
	deleted, errs := PurgeUploads(ctx, fs, time.Now(), true)
	if len(errs) > 0 {
		t.Errorf("Unexpected errors")
	}
	if len(deleted) > 0 {
		t.Errorf("Files unexpectedly deleted: %s", deleted)
	}
}
