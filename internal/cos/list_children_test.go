package cos

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// ListChildren asks COS to group keys by "/" and reads both halves of the
// answer, objects and common prefixes, across pages.
func TestListChildrenUsesTheDelimiter(t *testing.T) {
	page := func(token string, key, common string, next string) string {
		truncated := next != ""
		return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name><Prefix>dir/Al</Prefix><Delimiter>/</Delimiter><KeyCount>2</KeyCount>
  <IsTruncated>%t</IsTruncated><NextContinuationToken>%s</NextContinuationToken>
  <Contents><Key>%s</Key><Size>1</Size><LastModified>2026-09-30T12:00:00.000Z</LastModified><ETag>"e"</ETag></Contents>
  <CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>
</ListBucketResult>`, truncated, next, key, common)
	}
	var delimiters []string
	client := newLocalTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		delimiters = append(delimiters, q.Get("delimiter"))
		w.Header().Set("Content-Type", "application/xml")
		if q.Get("continuation-token") == "" {
			fmt.Fprint(w, page("", "dir/Alpha", "dir/Alps/", "t2"))
			return
		}
		fmt.Fprint(w, page("t2", "dir/alpine", "dir/also/", ""))
	}))

	objects, prefixes, err := client.ListChildren(context.Background(), "dir/Al", 0)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, obj := range objects {
		keys = append(keys, obj.Key)
	}
	if !slices.Equal(keys, []string{"dir/Alpha", "dir/alpine"}) {
		t.Errorf("objects = %q", keys)
	}
	if !slices.Equal(prefixes, []string{"dir/Alps/", "dir/also/"}) {
		t.Errorf("prefixes = %q", prefixes)
	}
	if !slices.Equal(delimiters, []string{"/", "/"}) {
		t.Errorf("delimiter sent = %q, want / on both pages", delimiters)
	}

	// maxKeys counts objects and prefixes together.
	delimiters = nil
	objects, prefixes, err = client.ListChildren(context.Background(), "dir/Al", 3)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(objects) + len(prefixes); n != 3 {
		t.Errorf("maxKeys 3 returned %d entries", n)
	}
}
