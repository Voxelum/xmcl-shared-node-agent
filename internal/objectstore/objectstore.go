package objectstore

import "time"

type ObjectInfo struct {
	Key          string
	LastModified time.Time
	Size         int64
}
