package miniohelper

import (
	"context"
	"log"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func InitializeMinio(ctx context.Context) (client_name *minio.Client, bucketName string, error error) {
	minioEndpoint := getenv("MINIO_ENDPOINT", "192.168.28.111:30350")
	minioAccess := getenv("MINIO_ACCESS_KEY", "nephio1234")
	minioSecret := getenv("MINIO_SECRET_KEY", "secret1234")
	minioBucket := getenv("MINIO_BUCKET", "checkpoints")

	minioClient, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioAccess, minioSecret, ""),
		Secure: false,
	})
	if err != nil {
		log.Fatalf("Failed to initialize MinIO client: %v", err)
	}

	exists, err := minioClient.BucketExists(ctx, minioBucket)
	if err != nil {
		log.Fatalf("Failed to check bucket: %v", err)
	}
	if !exists {
		if err := minioClient.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{}); err != nil {
			log.Fatalf("Failed to create bucket: %v", err)
		}
	}
	return minioClient, minioBucket, nil
}

func getenv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// func getenvInt(key string, defaultVal int) int {
// 	if v := os.Getenv(key); v != "" {
// 		if i, err := strconv.Atoi(v); err == nil {
// 			return i
// 		}
// 	}
// 	return defaultVal
// }

// func getenvDuration(key string, defaultVal time.Duration) time.Duration {
// 	if v := os.Getenv(key); v != "" {
// 		if d, err := time.ParseDuration(v); err == nil {
// 			return d
// 		}
// 	}
// 	return defaultVal
// }
