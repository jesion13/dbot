package minio

import (
	"context"
	"log/slog"
	"strings"

	"dbot/pkg/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Minio struct {
	*minio.Client
}

func NewMinioStore(ctx context.Context) (Minio, error) {
	ret := Minio{}
	host := config.MINIO_HOST
	accessKeyID := config.MINIO_ACCESS_KEY_ID
	secretKey := config.MINIO_SECRET_ACCESS_KEY

	var secure bool
	if config.ENV == config.Prod {
		secure = true
	}

	minioClient, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return ret, err
	}
	ret.Client = minioClient

	err = ret.createDefaultBucket(ctx)
	if err != nil {
		slog.Error(err.Error())
	}

	return ret, nil
}

func (m *Minio) createDefaultBucket(ctx context.Context) error {
	bucketName := config.MINIO_DBOT_BUCKET_NAME
	err := m.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: "any"})
	if err != nil {
		// Check to see if we already own this bucket (which happens if you run this twice)
		exists, errBucketExists := m.BucketExists(ctx, bucketName)
		if errBucketExists == nil && exists {
			slog.Debug("bucket exists", slog.String("name", bucketName))
			return nil
		}
		return err
	}

	slog.Info("Successfully created", slog.String("name", bucketName))

	return nil
}

// creates folder for each / in folder name
// example: name=dupa/123/dupa
// will create same structure in config.MINIO_DBOT_BUCKET
func (m Minio) CreateFolderStructure(ctx context.Context, name string) error {
	slog.Debug("CreateFolderStructure", slog.String("name", name))
	var currentPath strings.Builder
	for folder := range strings.SplitSeq(name, "/") {
		currentPath.WriteString(folder + "/")
		// _,err := m.StatObject(ctx,config.MINIO_DBOT_BUCKET_NAME,currentPath,minio.StatObjectOptions{})
		// if err == nil {
		// 	continue
		// }

		slog.Debug("CreateFolderStructure", slog.Any("currentPath", currentPath.String()))
		_, err := m.PutObject(ctx, config.MINIO_DBOT_BUCKET_NAME, currentPath.String(), nil, 0, minio.PutObjectOptions{})
		if err != nil {
			return err
		}

	}
	return nil
}
