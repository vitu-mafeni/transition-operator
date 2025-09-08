package miniohelper

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/minio/minio-go/v7"
)

// IsFileUploaded checks if a local file has already been uploaded to MinIO.
// Returns true if the file exists and the hash matches.

// FileExistsInMinio checks if a file exists in the given MinIO bucket
// by its filename (derived from localFilePath). Returns true if it exists.
func FileExistsInMinio(ctx context.Context, client *minio.Client, bucket, localFilePath string) (bool, error) {
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

	fmt.Printf("File %s already exists in bucket %s\n", fileName, bucket)
	return true, nil
}
