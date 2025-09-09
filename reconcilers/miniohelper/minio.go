package miniohelper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// IsFileUploaded checks if a local file has already been uploaded to MinIO.
// Returns true if the file exists and the hash matches.

// FileExistsInMinio checks if a file exists in the given MinIO bucket
// by its filename (derived from localFilePath). Returns true if it exists.
func FileExistsInMinio(ctx context.Context, client *minio.Client, bucket, localFilePath string) (bool, error) {
	log := logf.FromContext(ctx)
	fileName := filepath.Base(localFilePath)

	_, err := client.StatObject(ctx, bucket, fileName, minio.StatObjectOptions{})
	if err != nil {
		// If the object does not exist, StatObject returns an error
		// MinIO client wraps 404 in ErrorResponse
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return false, nil
		}
		return false, err
	}

	log.Info("File %s already exists in bucket %s\n", fileName, bucket)
	return true, nil
}

// download file from minio
func DownloadFileFromMinio(ctx context.Context, client *minio.Client, bucket, objectName, localFilePath string) error {
	log := logf.FromContext(ctx)
	// Open object stream
	obj, err := client.GetObject(ctx, bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to get object %q from bucket %q: %w", objectName, bucket, err)
	}
	defer obj.Close()

	// Create local file
	localFile, err := os.Create(localFilePath)
	if err != nil {
		return fmt.Errorf("failed to create local file %q: %w", localFilePath, err)
	}
	defer localFile.Close()

	// Copy content
	written, err := io.Copy(localFile, obj)
	if err != nil {
		return fmt.Errorf("failed to write object %q to local file %q: %w", objectName, localFilePath, err)
	}

	// Verify size
	if written == 0 {
		return fmt.Errorf("downloaded file is empty: object %q from bucket %q", objectName, bucket)
	}

	log.Info("Downloaded %s (%d bytes) from bucket %s to %s\n", objectName, written, bucket, localFilePath)
	return nil
}

func WaitForFileInMinio(ctx context.Context, client *minio.Client, bucket, localFilePath string, timeout, checkInterval time.Duration) error {
	log := logf.FromContext(ctx)
	fileName := filepath.Base(localFilePath)
	start := time.Now()

	log.Info("Waiting for file %s to appear in MinIO bucket %s\n", fileName, bucket)

	for {
		exists, err := FileExistsInMinio(ctx, client, bucket, localFilePath)
		if err != nil {
			return fmt.Errorf("error checking MinIO for %s: %w", fileName, err)
		}
		if exists {
			return nil // file is in MinIO
		}

		if time.Since(start) > timeout {
			return fmt.Errorf("timed out waiting for file %s to appear in MinIO bucket %s", fileName, bucket)
		}

		time.Sleep(checkInterval)
	}
}
