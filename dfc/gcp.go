// Package dfc provides distributed file-based cache with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2017, NVIDIA CORPORATION. All rights reserved.  *
 */
package dfc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/api/iterator"
)

//======
//
// types
//
//======
type gcpif struct {
}

//======
//
// global - FIXME: environ
//
//======
func getProjID() string {
	return os.Getenv("GOOGLE_CLOUD_PROJECT")
}

//======
//
// methods
//
//======
func (cloudif *gcpif) listbucket(w http.ResponseWriter, bucket string, msg *GetMsg) (errstr string) {
	glog.Infof("gcp listbucket %s", bucket)
	client, gctx, errstr := createclient()
	if errstr != "" {
		return
	}
	it := client.Bucket(bucket).Objects(gctx, nil)

	var reslist = BucketList{Entries: make([]*BucketEntry, 0, 1000)}
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		entry := &BucketEntry{}
		entry.Name = attrs.Name
		if strings.Contains(msg.GetProps, GetPropsSize) {
			entry.Size = attrs.Size
		}
		if strings.Contains(msg.GetProps, GetPropsBucket) {
			entry.Bucket = attrs.Bucket
		}
		if strings.Contains(msg.GetProps, GetPropsCtime) {
			t := attrs.Created
			switch msg.GetTimeFormat {
			case "":
				fallthrough
			case RFC822:
				entry.Ctime = t.Format(time.RFC822)
			default:
				entry.Ctime = t.Format(msg.GetTimeFormat)
			}
		}
		if strings.Contains(msg.GetProps, GetPropsChecksum) {
			entry.Checksum = hex.EncodeToString(attrs.MD5)
		}
		// TODO: other GetMsg props TBD

		reslist.Entries = append(reslist.Entries, entry)
	}
	if glog.V(3) {
		glog.Infof("listbucket count %d", len(reslist.Entries))
	}
	jsbytes, err := json.Marshal(reslist)
	assert(err == nil, err)
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsbytes)
	return
}

// Initialize and create storage client
func createclient() (*storage.Client, context.Context, string) {
	if getProjID() == "" {
		return nil, nil, "Failed to get ProjectID from GCP"
	}
	gctx := context.Background()
	client, err := storage.NewClient(gctx)
	if err != nil {
		return nil, nil, fmt.Sprintf("Failed to create client, err: %v", err)
	}
	return client, gctx, ""
}

func (cloudif *gcpif) getobj(fqn string, bucket string, objname string) (errstr string) {
	var (
		file *os.File
		size int64
	)
	client, gctx, errstr := createclient()
	if errstr != "" {
		return
	}
	o := client.Bucket(bucket).Object(objname)
	attrs, err := o.Attrs(gctx)
	if err != nil {
		return fmt.Sprintf("Failed to get attributes for object %s from bucket %s, err: %v",
			objname, bucket, err)
	}
	md5 := hex.EncodeToString(attrs.MD5)
	rc, err := o.NewReader(gctx)
	if err != nil {
		return fmt.Sprintf("Failed to create rc for object %s to file %s, err: %v",
			objname, fqn, err)
	}
	defer rc.Close()
	if file, errstr = initobj(fqn); errstr != "" {
		return
	}
	if size, errstr = getobjto_Md5(file, fqn, objname, md5, rc); errstr != "" {
		file.Close()
		return
	}
	stats := getstorstats()
	stats.add("bytesloaded", size)

	if err = file.Close(); err != nil {
		return fmt.Sprintf("Failed to close downloaded file %s, err: %v", fqn, err)
	}
	return ""
}

func (cloudif *gcpif) putobj(r *http.Request, fqn, bucket, objname, md5sum string) (errstr string) {
	size := r.ContentLength
	teebuf, b := Maketeerw(size, r.Body)
	client, gctx, errstr := createclient()
	if errstr != "" {
		return
	}
	wc := client.Bucket(bucket).Object(objname).NewWriter(gctx)

	if _, err := copyBuffer(wc, teebuf); err != nil {
		errstr = fmt.Sprintf("gcp: failed to upload %s (bucket %s), err: %v", objname, bucket, err)
		return
	}
	if err := wc.Close(); err != nil {
		errstr = fmt.Sprintf("gcp: Unexpected failure to close wc upon uploading %s (bucket %s), err: %v",
			objname, bucket, err)
		return
	}
	r.Body = ioutil.NopCloser(b)
	written, err := ReceiveFile(fqn, r.Body, md5sum)
	if err != nil {
		errstr = fmt.Sprintf("gcp: failed to receive %s (written %d), err : %v", fqn, written, err)
		return
	}
	if errstr = truncatefile(fqn, size); errstr != "" {
		return
	}
	if glog.V(3) {
		glog.Infof("gcp: PUT %s (bucket %s)", objname, bucket)
	}
	return
}

func (cloudif *gcpif) deleteobj(bucket, objname string) (errstr string) {
	client, gctx, errstr := createclient()
	if errstr != "" {
		return
	}
	o := client.Bucket(bucket).Object(objname)
	err := o.Delete(gctx)
	if err != nil {
		errstr = fmt.Sprintf("gcp: failed to delete %s (bucket %s), err: %v", objname, bucket, err)
		return
	}
	if glog.V(3) {
		glog.Infof("gcp: deleted %s (bucket %s)", objname, bucket)
	}
	return
}
