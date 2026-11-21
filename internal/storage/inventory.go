package storage

import (
	"context"
	"fmt"
	"net/url"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/overmindv/media/internal/inventory"
)

func (s *S3) InventoryPages(bucket, prefix string) inventory.Page {
	return func(ctx context.Context, after string, size int) ([]inventory.Entry, bool, error) {
		if size < 1 || size > 1000 {
			return nil, false, fmt.Errorf("inventory page size must be between 1 and 1000")
		}
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), Prefix: aws.String(prefix), StartAfter: aws.String(after),
			MaxKeys: aws.Int32(int32(size)), EncodingType: types.EncodingTypeUrl,
		})
		if err != nil {
			return nil, false, err
		}
		entries := make([]inventory.Entry, 0, len(page.Contents))
		for _, object := range page.Contents {
			key := aws.ToString(object.Key)
			if page.EncodingType == types.EncodingTypeUrl {
				key, err = url.QueryUnescape(key)
				if err != nil {
					return nil, false, fmt.Errorf("invalid encoded inventory key")
				}
			}
			entries = append(entries, inventory.Entry{Key: key, LastModified: aws.ToTime(object.LastModified)})
		}

		return entries, aws.ToBool(page.IsTruncated), nil
	}
}
