package store

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGalleryTagMatchingAndTimeRange(t *testing.T) {
	eachStore(t, mysqlTestLane, func(t *testing.T, db *Store) {
		ctx := context.Background()
		alice := testUser(t, db, "alice", false)
		bob := testUser(t, db, "bob", false)
		admin := testUser(t, db, "admin", true)
		testImage(t, db, "a1", alice, 1000)
		testImage(t, db, "a2", alice, 2000)
		testImage(t, db, "a3", alice, 3000)
		testImage(t, db, "a4", alice, 4000)
		testImage(t, db, "b1", bob, 5000)
		testImage(t, db, "admin1", admin, 6000)
		attach := func(imageID string, user int64, names ...string) {
			t.Helper()
			if _, err := db.AddImageTags(ctx, imageID, user, false, names); err != nil {
				t.Fatalf("tag %s %v: %v", imageID, names, err)
			}
		}
		attach("a1", alice, "red")
		attach("a2", alice, "red", "blue")
		attach("a3", alice, "blue")
		attach("a4", alice, "green")
		attach("b1", bob, "red")
		attach("admin1", admin, "red")

		filter := GalleryFilter{UserID: alice}
		page, err := db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); !reflect.DeepEqual(got, []string{"a4", "a3", "a2", "a1"}) || page.Total != 4 {
			t.Fatalf("unfiltered page = %v total %d", got, page.Total)
		}

		filter.Tags = []string{"red"}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); !reflect.DeepEqual(got, []string{"a2", "a1"}) || page.Total != 2 {
			t.Fatalf("all red page = %v total %d", got, page.Total)
		}

		filter.Tags = []string{"Red"}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Images) != 0 || page.Total != 0 {
			t.Fatalf("case-sensitive filter = %v total %d", pageIDs(page), page.Total)
		}
		filter.Tags = nil

		filter.Mode = TagMatchAny
		filter.Tags = []string{"blue", "red"}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); !reflect.DeepEqual(got, []string{"a3", "a2", "a1"}) || page.Total != 3 {
			t.Fatalf("any red/blue page = %v total %d", got, page.Total)
		}

		filter = GalleryFilter{UserID: alice, Tags: []string{"missing"}}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Images) != 0 || page.Total != 0 {
			t.Fatalf("unknown all filter = %v total %d", pageIDs(page), page.Total)
		}
		filter.Mode = TagMatchAny
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Images) != 0 || page.Total != 0 {
			t.Fatalf("unknown any filter = %v total %d", pageIDs(page), page.Total)
		}
		filter = GalleryFilter{UserID: alice, Tags: []string{"red", "missing"}, Mode: TagMatchAny}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Images) != 0 || page.Total != 0 {
			t.Fatalf("partially unknown any filter = %v total %d", pageIDs(page), page.Total)
		}

		filter = GalleryFilter{UserID: alice, Tags: []string{"red"}, From: millisPtr(2000), To: millisPtr(4000)}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); !reflect.DeepEqual(got, []string{"a2"}) || page.Total != 1 {
			t.Fatalf("half-open red page = %v total %d", got, page.Total)
		}
		filter = GalleryFilter{UserID: alice, Tags: []string{"blue", "green"}, Mode: TagMatchAny, From: millisPtr(2000), To: millisPtr(4000)}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); !reflect.DeepEqual(got, []string{"a3", "a2"}) || page.Total != 2 {
			t.Fatalf("half-open any page = %v total %d", got, page.Total)
		}
		filter = GalleryFilter{UserID: alice, Tags: []string{"red"}, From: millisPtr(2000), To: millisPtr(2000)}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Images) != 0 || page.Total != 0 {
			t.Fatalf("empty range page = %v total %d", pageIDs(page), page.Total)
		}

		filter = GalleryFilter{UserID: bob, Tags: []string{"red"}}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); !reflect.DeepEqual(got, []string{"b1"}) || page.Total != 1 {
			t.Fatalf("bob red page = %v total %d", got, page.Total)
		}

		filter = GalleryFilter{UserID: alice, All: true}
		if _, err := db.GalleryImages(ctx, filter, nil, 0); !errors.Is(err, ErrPermission) {
			t.Fatalf("non-admin all scope error = %v, want ErrPermission", err)
		}
		filter = GalleryFilter{UserID: admin, Admin: true, All: true}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); len(got) != 6 || page.Total != 6 {
			t.Fatalf("admin all page = %v total %d", got, page.Total)
		}
		filter.Tags = []string{"red"}
		page, err = db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(page); !reflect.DeepEqual(got, []string{"admin1"}) || page.Total != 1 {
			t.Fatalf("admin all red page = %v total %d", got, page.Total)
		}

		filter = GalleryFilter{UserID: alice, Mode: TagMatchMode("bogus")}
		if _, err := db.GalleryImages(ctx, filter, nil, 0); !errors.Is(err, ErrTagMode) {
			t.Fatalf("bad mode error = %v, want ErrTagMode", err)
		}
		filter = GalleryFilter{UserID: alice, Tags: []string{"  "}}
		if _, err := db.GalleryImages(ctx, filter, nil, 0); !errors.Is(err, ErrTagName) {
			t.Fatalf("bad tag name error = %v, want ErrTagName", err)
		}
	})
}

func TestTimelineUTCDayBuckets(t *testing.T) {
	eachStore(t, mysqlTestLane, func(t *testing.T, db *Store) {
		ctx := context.Background()
		alice := testUser(t, db, "alice", false)
		bob := testUser(t, db, "bob", false)
		base := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC).UnixMilli()
		hour := int64(60 * 60 * 1000)
		n1 := base + dayMilliseconds
		n2 := n1 + 5*hour
		n3 := base + 2*dayMilliseconds - 1
		n2Start := base + 2*dayMilliseconds
		testImage(t, db, "o1", alice, base)
		testImage(t, db, "o2", alice, base+dayMilliseconds-1)
		testImage(t, db, "n1", alice, n1)
		testImage(t, db, "n2", alice, n2)
		testImage(t, db, "n3", alice, n3)
		testImage(t, db, "d2", alice, n2Start)
		testImage(t, db, "b1", bob, n2)
		if _, err := db.AddImageTags(ctx, "n2", alice, false, []string{"red"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AddImageTags(ctx, "n3", alice, false, []string{"red"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AddImageTags(ctx, "b1", bob, false, []string{"red"}); err != nil {
			t.Fatal(err)
		}
		october := TimelineBucket{Day: "2026-10-31", Start: base, End: base + dayMilliseconds, Count: 2}
		november := TimelineBucket{Day: "2026-11-01", Start: n1, End: n2Start, Count: 3}
		december := TimelineBucket{Day: "2026-11-02", Start: n2Start, End: n2Start + dayMilliseconds, Count: 1}

		filter := GalleryFilter{UserID: alice}
		buckets, err := db.TimelineBuckets(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if want := []TimelineBucket{december, november, october}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("all buckets = %+v, want %+v", buckets, want)
		}

		filter = GalleryFilter{UserID: alice, From: millisPtr(n1), To: millisPtr(n2Start)}
		buckets, err = db.TimelineBuckets(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if want := []TimelineBucket{november}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("half-open buckets = %+v, want %+v", buckets, want)
		}
		filter.From = millisPtr(n1 + 1)
		buckets, err = db.TimelineBuckets(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		want := november
		want.Count = 2
		if want := []TimelineBucket{want}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("midnight-exclusive buckets = %+v, want %+v", buckets, want)
		}
		filter = GalleryFilter{UserID: alice, To: millisPtr(n1)}
		buckets, err = db.TimelineBuckets(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if want := []TimelineBucket{october}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("to-exclusive buckets = %+v, want %+v", buckets, want)
		}

		filter = GalleryFilter{UserID: alice, Tags: []string{"red"}}
		buckets, err = db.TimelineBuckets(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		want = november
		want.Count = 2
		if want := []TimelineBucket{want}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("tag buckets = %+v, want %+v", buckets, want)
		}
		filter = GalleryFilter{UserID: bob, Tags: []string{"red"}}
		buckets, err = db.TimelineBuckets(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		want = november
		want.Count = 1
		if want := []TimelineBucket{want}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("bob buckets = %+v, want %+v", buckets, want)
		}
		filter = GalleryFilter{UserID: alice, Tags: []string{"missing"}}
		buckets, err = db.TimelineBuckets(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if len(buckets) != 0 {
			t.Fatalf("unknown tag buckets = %+v", buckets)
		}

		if _, err := db.DeleteImage("n2", alice, false); err != nil {
			t.Fatal(err)
		}
		buckets, err = db.TimelineBuckets(ctx, GalleryFilter{UserID: alice})
		if err != nil {
			t.Fatal(err)
		}
		want = november
		want.Count = 2
		if want := []TimelineBucket{december, want, october}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("buckets after image delete = %+v, want %+v", buckets, want)
		}
		if _, err := db.AddImageTags(ctx, "n3", alice, false, []string{"extra"}); err != nil {
			t.Fatal(err)
		}
		buckets, err = db.TimelineBuckets(ctx, GalleryFilter{UserID: alice, Tags: []string{"extra"}})
		if err != nil {
			t.Fatal(err)
		}
		want = november
		want.Count = 1
		if want := []TimelineBucket{want}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("realtime tag buckets = %+v, want %+v", buckets, want)
		}

		testImage(t, db, "t1", alice, n2Start)
		testImage(t, db, "t2", alice, n2Start)
		buckets, err = db.TimelineBuckets(ctx, GalleryFilter{UserID: alice})
		if err != nil {
			t.Fatal(err)
		}
		tied := december
		tied.Count = 3
		after := november
		after.Count = 2
		if want := []TimelineBucket{tied, after, october}; !reflect.DeepEqual(buckets, want) {
			t.Fatalf("tie buckets = %+v, want %+v", buckets, want)
		}

		buckets, err = db.TimelineBuckets(ctx, GalleryFilter{UserID: alice, From: millisPtr(n2Start), To: millisPtr(n2Start)})
		if err != nil {
			t.Fatal(err)
		}
		if len(buckets) != 0 {
			t.Fatalf("empty range buckets = %+v", buckets)
		}
	})
}

func TestGalleryKeysetPaginationStability(t *testing.T) {
	eachStore(t, mysqlTestLane, func(t *testing.T, db *Store) {
		ctx := context.Background()
		user := testUser(t, db, "pager", false)
		testImage(t, db, "a", user, 4000)
		testImage(t, db, "b", user, 4000)
		testImage(t, db, "c", user, 4000)
		testImage(t, db, "d", user, 4000)
		testImage(t, db, "e", user, 3000)
		testImage(t, db, "f", user, 5000)
		filter := GalleryFilter{UserID: user}

		first, err := db.GalleryImages(ctx, filter, nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(first); !reflect.DeepEqual(got, []string{"f", "d"}) || first.Total != 6 || first.Limit != 2 {
			t.Fatalf("first page = %v total %d limit %d", got, first.Total, first.Limit)
		}
		if first.Next == nil || first.Next.CreatedAt != 4000 || first.Next.ID != "d" {
			t.Fatalf("first next = %+v", first.Next)
		}

		testImage(t, db, "g", user, 4000)
		testImage(t, db, "h", user, 6000)
		second, err := db.GalleryImages(ctx, filter, first.Next, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(second); !reflect.DeepEqual(got, []string{"c", "b"}) || second.Total != 8 {
			t.Fatalf("second page = %v total %d", got, second.Total)
		}
		if second.Next == nil || second.Next.CreatedAt != 4000 || second.Next.ID != "b" {
			t.Fatalf("second next = %+v", second.Next)
		}
		third, err := db.GalleryImages(ctx, filter, second.Next, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got := pageIDs(third); !reflect.DeepEqual(got, []string{"a", "e"}) || third.Total != 8 {
			t.Fatalf("third page = %v total %d", got, third.Total)
		}
		if third.Next != nil {
			t.Fatalf("third next = %+v, want nil", third.Next)
		}

		defaulted, err := db.GalleryImages(ctx, filter, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		if defaulted.Limit != DefaultGalleryLimit || len(defaulted.Images) != 8 {
			t.Fatalf("default page limit %d len %d", defaulted.Limit, len(defaulted.Images))
		}
		capped, err := db.GalleryImages(ctx, filter, nil, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if capped.Limit != MaxGalleryLimit {
			t.Fatalf("capped page limit = %d", capped.Limit)
		}
		negative, err := db.GalleryImages(ctx, filter, nil, -3)
		if err != nil {
			t.Fatal(err)
		}
		if negative.Limit != DefaultGalleryLimit {
			t.Fatalf("negative page limit = %d", negative.Limit)
		}

		stale, err := db.GalleryImages(ctx, filter, &GalleryCursor{CreatedAt: 0, ID: "zzz"}, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(stale.Images) != 0 || stale.Total != 8 || stale.Next != nil {
			t.Fatalf("stale page = %v total %d next %+v", pageIDs(stale), stale.Total, stale.Next)
		}
		if _, err := db.GalleryImages(ctx, filter, &GalleryCursor{CreatedAt: 1}, 2); !errors.Is(err, ErrGalleryCursor) {
			t.Fatalf("empty cursor error = %v, want ErrGalleryCursor", err)
		}
	})
}

func TestGalleryCursorEncoding(t *testing.T) {
	cursor := GalleryCursor{CreatedAt: 1700000000123, ID: strings.Repeat("a", 32)}
	encoded := EncodeGalleryCursor(cursor)
	decoded, err := DecodeGalleryCursor(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != cursor {
		t.Fatalf("cursor roundtrip = %+v, want %+v", decoded, cursor)
	}
	if strings.ContainsAny(encoded, "+/=") {
		t.Fatalf("cursor %q is not URL-safe and unpadded", encoded)
	}
	invalid := []string{
		"",
		"!!!",
		base64.RawURLEncoding.EncodeToString([]byte("123")),
		base64.RawURLEncoding.EncodeToString([]byte(":abc")),
		base64.RawURLEncoding.EncodeToString([]byte("x:abc")),
		base64.RawURLEncoding.EncodeToString([]byte("123:")),
		base64.RawURLEncoding.EncodeToString([]byte("123:" + strings.Repeat("a", 129))),
	}
	for _, value := range invalid {
		if _, err := DecodeGalleryCursor(value); !errors.Is(err, ErrGalleryCursor) {
			t.Fatalf("DecodeGalleryCursor(%q) error = %v, want ErrGalleryCursor", value, err)
		}
	}
}

func millisPtr(value int64) *int64 {
	return &value
}

func pageIDs(page GalleryPage) []string {
	ids := make([]string, 0, len(page.Images))
	for _, image := range page.Images {
		ids = append(ids, image.ID)
	}
	return ids
}
