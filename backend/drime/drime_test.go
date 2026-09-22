// Drime filesystem interface
package drime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/drime/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/fstest/fstests"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"github.com/stretchr/testify/require"
)

func TestShouldUsePresignedUpload(t *testing.T) {
	const fiveMiB = int64(5 * fs.Mebi)
	for _, test := range []struct {
		name    string
		enabled bool
		cutoff  int64
		size    int64
		want    bool
	}{
		{name: "disabled", cutoff: int64(10 * fs.Mebi), size: 1, want: false},
		{name: "unknown size", enabled: true, cutoff: int64(10 * fs.Mebi), size: -1, want: false},
		{name: "below limits", enabled: true, cutoff: int64(10 * fs.Mebi), size: fiveMiB - 1, want: true},
		{name: "at presign limit", enabled: true, cutoff: int64(10 * fs.Mebi), size: fiveMiB, want: false},
		{name: "at cutoff", enabled: true, cutoff: int64(2 * fs.Mebi), size: int64(2 * fs.Mebi), want: true},
		{name: "above cutoff", enabled: true, cutoff: int64(2 * fs.Mebi), size: int64(2*fs.Mebi + 1), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := Fs{opt: Options{
				UsePresignedUploads: test.enabled,
				UploadCutoff:        fs.SizeSuffix(test.cutoff),
			}}
			require.Equal(t, test.want, f.shouldUsePresignedUpload(test.size))
		})
	}
}

func TestPathHasExactSegment(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "missing/0/deep/file.bin", want: true},
		{path: "/missing/0/deep/file.bin", want: true},
		{path: "0", want: true},
		{path: "missing/0000/deep/file.bin", want: false},
		{path: "missing/10/deep/file.bin", want: false},
		{path: "missing/00a0/deep/file.bin", want: false},
	} {
		t.Run(test.path, func(t *testing.T) {
			require.Equal(t, test.want, pathHasExactSegment(test.path, "0"))
		})
	}
}

func TestUploadPathCoordinatorWaitsForOverlappingPrefix(t *testing.T) {
	coordinator := newUploadPathCoordinator()
	finishFirst, err := coordinator.begin(context.Background(), "safe/x")
	require.NoError(t, err)

	started := make(chan struct{})
	type result struct {
		finish func(bool)
		err    error
	}
	acquired := make(chan result, 1)
	go func() {
		close(started)
		finish, err := coordinator.begin(context.Background(), "safe/0")
		acquired <- result{finish: finish, err: err}
	}()
	<-started

	select {
	case <-acquired:
		t.Fatal("overlapping path acquired before the shared prefix was ready")
	case <-time.After(50 * time.Millisecond):
	}

	finishFirst(true)
	select {
	case second := <-acquired:
		require.NoError(t, second.err)
		second.finish(true)
	case <-time.After(time.Second):
		t.Fatal("overlapping path did not resume after the shared prefix became ready")
	}
}

func TestUploadPresigned(t *testing.T) {
	const (
		contents      = "hello"
		authorization = "Bearer secret"
		filename      = "371add24d1b9ee47aa4912e2a5f9f608"
	)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/s3/simple/presign":
			require.Equal(t, authorization, r.Header.Get("Authorization"))
			var request map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, map[string]any{
				"extension":   "bin",
				"filename":    filename,
				"mime":        "text/plain",
				"parentId":    float64(42),
				"size":        float64(len(contents)),
				"workspaceId": float64(0),
			}, request)
			_, err := w.Write([]byte(`{"url":"` + server.URL + `/storage/object","key":"uploads/uuid/uuid","status":"success"}`))
			require.NoError(t, err)
		case "/storage/object":
			require.Equal(t, http.MethodPut, r.Method)
			require.Empty(t, r.Header.Get("Authorization"))
			require.Equal(t, "text/plain", r.Header.Get("Content-Type"))
			require.Equal(t, int64(len(contents)), r.ContentLength)
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, contents, string(body))
		case "/s3/entries":
			require.Equal(t, authorization, r.Header.Get("Authorization"))
			var request map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, map[string]any{
				"clientExtension": "bin",
				"clientMime":      "text/plain",
				"clientName":      filename,
				"filename":        "uuid",
				"parentId":        float64(42),
				"relativePath":    filename,
				"size":            float64(len(contents)),
				"workspaceId":     float64(0),
			}, request)
			_, err := w.Write([]byte(`{"fileEntry":{"id":123,"name":"` + filename + `","file_size":5,"mime":"text/plain"},"status":"success"}`))
			require.NoError(t, err)
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := rest.NewClient(server.Client()).SetRoot(server.URL)
	client.SetHeader("Authorization", authorization)
	f := &Fs{
		opt:   Options{},
		srv:   client,
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	o := &Object{fs: f, remote: filename}
	src := object.NewStaticObjectInfo(filename, time.Now(), int64(len(contents)), true, nil, nil).WithMimeType("text/plain")
	require.NoError(t, o.uploadPresigned(context.Background(), bytes.NewBufferString(contents), src, filename, "42", filename))
	require.Equal(t, "123", o.id)
}

func TestUploadAndVerify(t *testing.T) {
	const (
		contents = "hello"
		hash     = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/file-entries/123/verify-integrity", r.URL.Path)
		var request api.VerifyIntegrityRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, hash, request.SHA256)
		_, err := w.Write([]byte(`{"verified":true,"serverHash":"` + hash + `"}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{VerifyUploads: true},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	o := &Object{fs: f}
	err := o.uploadAndVerify(context.Background(), bytes.NewBufferString(contents), func(in io.Reader) error {
		_, err := io.Copy(io.Discard, in)
		o.id = "123"
		return err
	})
	require.NoError(t, err)
}

func TestVerifyIntegrityMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`{"verified":false,"serverHash":"wrong"}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	o := &Object{fs: f, id: "123"}
	require.ErrorContains(t, o.verifyIntegrity(context.Background(), "expected"), "upload integrity check failed")
}

func TestPresignedBatchAPIs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/s3/simple/presign-batch":
			var request api.SimpleUploadPresignBatchRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Len(t, request.Files, 2)
			_, err := w.Write([]byte(`{"files":[{"url":"https://storage/1","key":"uploads/1"},{"url":"https://storage/2","key":"uploads/2"}],"status":"success"}`))
			require.NoError(t, err)
		case "/s3/entries/batch":
			var request api.S3EntriesBatchRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Len(t, request.Files, 2)
			_, err := w.Write([]byte(`{"results":[{"index":1,"status":422,"error":"bad entry"},{"index":0,"status":200,"fileEntry":{"id":123,"name":"one"}}],"status":"success"}`))
			require.NoError(t, err)
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	f := &Fs{
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	presignItems := []api.SimpleUploadPresignRequest{{Filename: "one"}, {Filename: "two"}}
	presignResults := make([]api.SimpleUploadPresignResponse, len(presignItems))
	presignErrors := make([]error, len(presignItems))
	require.NoError(t, f.commitPresignBatch(context.Background(), presignItems, presignResults, presignErrors))
	require.Equal(t, "uploads/1", presignResults[0].Key)
	require.Equal(t, "uploads/2", presignResults[1].Key)
	require.NoError(t, presignErrors[0])
	require.NoError(t, presignErrors[1])

	entryItems := []api.S3EntriesRequest{{ClientName: "one"}, {ClientName: "two"}}
	entryResults := make([]api.Item, len(entryItems))
	entryErrors := make([]error, len(entryItems))
	require.NoError(t, f.commitEntriesBatch(context.Background(), entryItems, entryResults, entryErrors))
	require.Equal(t, "123", entryResults[0].ID.String())
	require.NoError(t, entryErrors[0])
	require.ErrorContains(t, entryErrors[1], "status 422: bad entry")
}

func TestListAllFailsWhenPaginationDoesNotAdvance(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		page := r.URL.Query().Get("page")
		if page == "1" {
			_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[]}`))
			require.NoError(t, err)
			return
		}
		require.Equal(t, "2", page)
		_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[]}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{ListChunk: 200},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	_, err := f.listAll(context.Background(), "42", false, false, "", func(*api.Item) bool { return false })
	require.ErrorContains(t, err, "pagination did not advance: requested page 2, received page 1")
	require.Equal(t, 2, requests)
}

func TestListAllUsesNameQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "42", r.URL.Query().Get("folderId"))
		require.Equal(t, "wanted", r.URL.Query().Get("query"))
		_, err := w.Write([]byte(`{"current_page":1,"last_page":1,"data":[{"id":7,"parent_id":42,"name":"wanted","type":"folder"}]}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{ListChunk: 200},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	found, err := f.listAll(context.Background(), "42", true, false, "wanted", func(item *api.Item) bool {
		return item.Name == "wanted"
	})
	require.NoError(t, err)
	require.True(t, found)
}

func TestListRUsesParentIDBatches(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/drive/file-entries", r.URL.Path)
		require.Empty(t, r.URL.Query().Get("folderId"))

		var data string
		switch r.URL.Query().Get("parentIds") {
		case "1":
			data = `[
				{"id":2,"parent_id":1,"name":"dir","type":"folder","updated_at":"2026-01-01T00:00:00Z"},
				{"id":5,"parent_id":1,"name":"other","type":"folder","updated_at":"2026-01-01T00:00:00Z"},
				{"id":3,"parent_id":1,"name":"root.txt","type":"file","file_size":4,"updated_at":"2026-01-01T00:00:00Z"}
			]`
		case "2,5":
			if r.URL.Query().Get("filters") != "" {
				cursor := time.Date(2026, 1, 1, 0, 0, 4, 0, time.UTC)
				wantFilter, err := encodeListingFilters(
					listingFilter{Key: "created_at", Value: cursor.Format(time.RFC3339Nano), Operator: ">="},
					listingFilter{Key: "parent_id", Value: []string{"2", "5"}, Operator: "in"},
				)
				require.NoError(t, err)
				require.Equal(t, wantFilter, r.URL.Query().Get("filters"))
				_, err = w.Write([]byte(`{"current_page":1,"last_page":1,"data":[
					{"id":4,"parent_id":2,"name":"nested.txt","type":"file","file_size":6,"created_at":"2026-01-01T00:00:04Z","updated_at":"2026-01-01T00:00:00Z"},
					{"id":6,"parent_id":5,"name":"another.txt","type":"file","file_size":7,"created_at":"2026-01-01T00:00:05Z","updated_at":"2026-01-01T00:00:00Z"}
				]}`))
				require.NoError(t, err)
				return
			}
			if r.URL.Query().Get("page") == "1" {
				_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[{"id":4,"parent_id":2,"name":"nested.txt","type":"file","file_size":6,"created_at":"2026-01-01T00:00:04Z","updated_at":"2026-01-01T00:00:00Z"}]}`))
				require.NoError(t, err)
				return
			}
			_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[]}`))
			require.NoError(t, err)
			return
		case "2":
			data = `[
				{"id":4,"parent_id":2,"name":"nested.txt","type":"file","file_size":6,"updated_at":"2026-01-01T00:00:00Z"}
			]`
		case "5":
			data = `[
				{"id":6,"parent_id":5,"name":"another.txt","type":"file","file_size":7,"updated_at":"2026-01-01T00:00:00Z"}
			]`
		default:
			t.Fatalf("unexpected parentIds %q", r.URL.Query().Get("parentIds"))
		}
		_, err := w.Write([]byte(`{"current_page":1,"last_page":1,"data":` + data + `}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{ListChunk: 200, ListParentBatchSize: 100},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	f.dirCache = dircache.New("", "1", f)

	var remotes []string
	err := f.ListR(context.Background(), "", func(entries fs.DirEntries) error {
		for _, entry := range entries {
			remotes = append(remotes, entry.Remote())
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"dir", "other", "root.txt", "dir/nested.txt", "other/another.txt"}, remotes)
	require.Equal(t, 4, requests)
}

func TestListRContinuesSingleParentByCreatedAt(t *testing.T) {
	requests := 0
	cursor := time.Date(2026, 1, 1, 0, 0, 4, 0, time.UTC)
	wantFilter, err := encodeListingFilters(
		listingFilter{Key: "created_at", Value: cursor.Format(time.RFC3339Nano), Operator: ">="},
		listingFilter{Key: "parent_id", Value: "9", Operator: "="},
	)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "9", r.URL.Query().Get("parentIds"))
		require.Equal(t, "created_at", r.URL.Query().Get("orderBy"))
		require.Equal(t, "asc", r.URL.Query().Get("orderDir"))
		if r.URL.Query().Get("filters") != "" {
			require.Equal(t, wantFilter, r.URL.Query().Get("filters"))
			require.Equal(t, "1", r.URL.Query().Get("page"))
			_, err := w.Write([]byte(`{"current_page":1,"last_page":1,"data":[
				{"id":4,"parent_id":9,"created_at":"2026-01-01T00:00:04Z"},
				{"id":5,"parent_id":9,"created_at":"2026-01-01T00:00:05Z"}
			]}`))
			require.NoError(t, err)
			return
		}
		require.Equal(t, "2", r.URL.Query().Get("perPage"))
		switch r.URL.Query().Get("page") {
		case "1":
			_, err := w.Write([]byte(`{"current_page":1,"last_page":3,"data":[
				{"id":1,"parent_id":9,"created_at":"2026-01-01T00:00:01Z"},
				{"id":2,"parent_id":9,"created_at":"2026-01-01T00:00:02Z"}
			]}`))
			require.NoError(t, err)
		case "2":
			_, err := w.Write([]byte(`{"current_page":2,"last_page":3,"data":[
				{"id":3,"parent_id":9,"created_at":"2026-01-01T00:00:03Z"},
				{"id":4,"parent_id":9,"created_at":"2026-01-01T00:00:04Z"}
			]}`))
			require.NoError(t, err)
		default:
			_, err := w.Write([]byte(`{"current_page":2,"last_page":3,"data":[]}`))
			require.NoError(t, err)
		}
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{ListChunk: 2},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	items, err := f.listAllParentsWithFallback(context.Background(), []string{"9"})
	require.NoError(t, err)
	require.Len(t, items, 5)
	gotIDs := make([]string, len(items))
	for i := range items {
		gotIDs[i] = items[i].ID.String()
	}
	require.Equal(t, []string{"1", "2", "3", "4", "5"}, gotIDs)
	require.Equal(t, 4, requests)
}

func TestListContinuesSingleParentByCreatedAt(t *testing.T) {
	requests := 0
	cursor := time.Date(2026, 1, 1, 0, 0, 2, 0, time.UTC)
	wantFilter, err := encodeListingFilters(
		listingFilter{Key: "created_at", Value: cursor.Format(time.RFC3339Nano), Operator: ">="},
		listingFilter{Key: "parent_id", Value: "9", Operator: "="},
	)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		require.Equal(t, "9", r.URL.Query().Get("parentIds"))
		if r.URL.Query().Get("filters") != "" {
			require.Equal(t, wantFilter, r.URL.Query().Get("filters"))
			_, err := w.Write([]byte(`{"current_page":1,"last_page":1,"data":[
				{"id":2,"parent_id":9,"name":"two","type":"folder","created_at":"2026-01-01T00:00:02Z","updated_at":"2026-01-01T00:00:00Z"},
				{"id":3,"parent_id":9,"name":"three","type":"file","file_size":3,"created_at":"2026-01-01T00:00:03Z","updated_at":"2026-01-01T00:00:00Z"}
			]}`))
			require.NoError(t, err)
			return
		}
		switch r.URL.Query().Get("page") {
		case "1":
			_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[
				{"id":1,"parent_id":9,"name":"one","type":"folder","created_at":"2026-01-01T00:00:01Z","updated_at":"2026-01-01T00:00:00Z"},
				{"id":2,"parent_id":9,"name":"two","type":"folder","created_at":"2026-01-01T00:00:02Z","updated_at":"2026-01-01T00:00:00Z"}
			]}`))
			require.NoError(t, err)
		default:
			_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[]}`))
			require.NoError(t, err)
		}
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{ListChunk: 2},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	f.dirCache = dircache.New("", "9", f)
	entries, err := f.List(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, []string{"one", "two", "three"}, []string{entries[0].Remote(), entries[1].Remote(), entries[2].Remote()})
	require.Equal(t, 3, requests)
}

func TestListRStopsWhenCreatedAtCursorDoesNotAdvance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "1" {
			id := "1"
			if r.URL.Query().Get("filters") != "" {
				id = "2"
			}
			_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[{"id":` + id + `,"parent_id":9,"created_at":"2026-01-01T00:00:01Z"}]}`))
			require.NoError(t, err)
			return
		}
		_, err := w.Write([]byte(`{"current_page":1,"last_page":2,"data":[]}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{ListChunk: 1},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	_, err := f.listAllParentsWithFallback(context.Background(), []string{"9"})
	require.ErrorContains(t, err, "pagination creation-time cursor did not advance")
}

func TestListAllDetectsRepeatedPageEntries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		_, err := w.Write([]byte(`{"current_page":` + page + `,"last_page":3,"data":[{"id":1},{"id":2}]}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		opt:   Options{ListChunk: 2},
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	_, err := f.listAll(context.Background(), "9", false, false, "", func(*api.Item) bool { return false })
	require.ErrorContains(t, err, "pagination repeated the entries from page 1")
}

func TestCleanUp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/file-entries/delete", r.URL.Path)

		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.Equal(t, map[string]any{
			"emptyTrash": true,
			"entryIds":   []any{},
		}, request)

		_, err := w.Write([]byte(`{"status":"success"}`))
		require.NoError(t, err)
	}))
	defer server.Close()

	f := &Fs{
		srv:   rest.NewClient(server.Client()).SetRoot(server.URL),
		pacer: fs.NewPacer(context.Background(), pacer.NewDefault()),
	}
	require.NoError(t, f.CleanUp(context.Background()))
}

// TestIntegration runs integration tests against the remote
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestDrime:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			MinChunkSize: minChunkSize,
		},
	})
}

func (f *Fs) SetUploadChunkSize(cs fs.SizeSuffix) (fs.SizeSuffix, error) {
	return f.setUploadChunkSize(cs)
}

func (f *Fs) SetUploadCutoff(cs fs.SizeSuffix) (fs.SizeSuffix, error) {
	return f.setUploadCutoff(cs)
}

var (
	_ fstests.SetUploadChunkSizer = (*Fs)(nil)
	_ fstests.SetUploadCutoffer   = (*Fs)(nil)
)
