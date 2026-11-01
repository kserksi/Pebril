package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"

	"github.com/kserksi/Pebril/internal/config"
	"github.com/kserksi/Pebril/internal/testutil"
)

func TestNormalizeTagName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		invalid bool
	}{
		{name: "plain", input: "Travel", want: "Travel"},
		{name: "case preserved", input: "tRaVeL", want: "tRaVeL"},
		{name: "trim ascii", input: " \tTravel\r\n", want: "Travel"},
		{name: "trim newlines around", input: "\n Travel \t", want: "Travel"},
		{name: "collapse spaces", input: "New    York", want: "New York"},
		{name: "collapse tabs and spaces", input: "a \t  b\tc", want: "a b c"},
		{name: "nfc composed", input: "Cafe\u0301", want: "Café"},
		{name: "nfc after trim", input: "  e\u0301  ", want: "é"},
		{name: "nbsp is not ascii whitespace", input: "a\u00a0b", want: "a\u00a0b"},
		{name: "32 code points", input: strings.Repeat("é", TagMaxCodePoints), want: strings.Repeat("é", TagMaxCodePoints)},
		{name: "empty", input: "", invalid: true},
		{name: "whitespace only", input: " \t \r\n", invalid: true},
		{name: "interior newline is control", input: "a\nb", invalid: true},
		{name: "nul is control", input: "a\x00b", invalid: true},
		{name: "del is control", input: "a\x7fb", invalid: true},
		{name: "invalid utf8", input: "\xff", invalid: true},
		{name: "33 code points", input: strings.Repeat("é", TagMaxCodePoints+1), invalid: true},
		{name: "reserved leading hash", input: "#travel", invalid: true},
		{name: "hash only", input: "#", invalid: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTagName(tc.input)
			if tc.invalid {
				if !errors.Is(err, ErrTagName) {
					t.Fatalf("NormalizeTagName(%q) error = %v, want ErrTagName", tc.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTagName(%q) error = %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeTagName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestTagLibraryIsolationAndCounts(t *testing.T) {
	eachStore(t, mysqlTestLane, func(t *testing.T, db *Store) {
		ctx := context.Background()
		alice := testUser(t, db, "alice", false)
		bob := testUser(t, db, "bob", false)
		testImage(t, db, "a1", alice, 1000)
		testImage(t, db, "a2", alice, 2000)
		testImage(t, db, "b1", bob, 3000)

		travel, err := db.EnsureTag(ctx, alice, " Travel ")
		if err != nil {
			t.Fatal(err)
		}
		if travel.Name != "Travel" || travel.UserID != alice || travel.Count != 0 {
			t.Fatalf("EnsureTag = %+v", travel)
		}
		again, err := db.EnsureTag(ctx, alice, "Travel")
		if err != nil {
			t.Fatal(err)
		}
		if again.ID != travel.ID {
			t.Fatalf("EnsureTag create = %+v, second call = %+v", travel, again)
		}
		bobTravel, err := db.EnsureTag(ctx, bob, "Travel")
		if err != nil {
			t.Fatal(err)
		}
		if bobTravel.ID == travel.ID {
			t.Fatalf("same name across users shared tag id %d", travel.ID)
		}
		lower, err := db.EnsureTag(ctx, alice, "travel")
		if err != nil {
			t.Fatal(err)
		}
		if lower.ID == travel.ID {
			t.Fatalf("case-different names shared tag id %d", travel.ID)
		}

		if _, err := db.AddImageTags(ctx, "a1", alice, false, []string{"Travel", "Travel"}); err != nil {
			t.Fatal(err)
		}
		tags, err := db.ImageTags(ctx, "a1", alice, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := imageTagNames(tags); len(got) != 1 || got[0] != "Travel" {
			t.Fatalf("ImageTags after duplicate add = %v", got)
		}
		if _, err := db.AddImageTags(ctx, "a2", alice, false, []string{"Travel"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AddImageTags(ctx, "b1", bob, false, []string{"Travel"}); err != nil {
			t.Fatal(err)
		}

		library, err := db.ListTags(ctx, alice)
		if err != nil {
			t.Fatal(err)
		}
		counts := libraryCounts(library)
		if counts["Travel"] != 2 {
			t.Fatalf("alice Travel count = %d, want 2; library = %+v", counts["Travel"], library)
		}
		if _, ok := counts["travel"]; !ok || counts["travel"] != 0 {
			t.Fatalf("case-different zero-count tag missing; library = %+v", library)
		}
		bobLibrary, err := db.ListTags(ctx, bob)
		if err != nil {
			t.Fatal(err)
		}
		if got := libraryCounts(bobLibrary)["Travel"]; got != 1 {
			t.Fatalf("bob Travel count = %d, want 1", got)
		}
		fetched, err := db.EnsureTag(ctx, alice, "Travel")
		if err != nil {
			t.Fatal(err)
		}
		if fetched.Count != 2 {
			t.Fatalf("EnsureTag existing count = %d, want 2", fetched.Count)
		}

		if err := db.DeleteTag(ctx, alice, "Travel"); err != nil {
			t.Fatal(err)
		}
		tags, err = db.ImageTags(ctx, "a1", alice, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != 0 {
			t.Fatalf("detached image still has tags %v", imageTagNames(tags))
		}
		if _, err := db.Image("a1"); err != nil {
			t.Fatalf("DeleteTag removed an image: %v", err)
		}
		library, err = db.ListTags(ctx, alice)
		if err != nil {
			t.Fatal(err)
		}
		counts = libraryCounts(library)
		if _, ok := counts["Travel"]; ok {
			t.Fatalf("deleted tag still listed: %+v", library)
		}
		if _, ok := counts["travel"]; !ok || counts["travel"] != 0 {
			t.Fatalf("untouched zero-count tag missing: %+v", library)
		}
		bobLibrary, err = db.ListTags(ctx, bob)
		if err != nil {
			t.Fatal(err)
		}
		if got := libraryCounts(bobLibrary)["Travel"]; got != 1 {
			t.Fatalf("alice DeleteTag changed bob's count to %d", got)
		}
		bobTags, err := db.ImageTags(ctx, "b1", bob, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := imageTagNames(bobTags); len(got) != 1 || got[0] != "Travel" {
			t.Fatalf("alice DeleteTag changed bob's image tags: %v", got)
		}
		if err := db.DeleteTag(ctx, alice, "Travel"); !errors.Is(err, ErrTagNotFound) {
			t.Fatalf("second DeleteTag error = %v, want ErrTagNotFound", err)
		}
		if err := db.DeleteTag(ctx, bob, "travel"); !errors.Is(err, ErrTagNotFound) {
			t.Fatalf("cross-library DeleteTag error = %v, want ErrTagNotFound", err)
		}

		if _, err := db.AddImageTags(ctx, "a1", alice, false, []string{"travel"}); err != nil {
			t.Fatal(err)
		}
		voyage, err := db.EnsureTag(ctx, alice, "Voyage")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.AddImageTags(ctx, "a1", alice, false, []string{voyage.Name}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RemoveImageTag(ctx, "a1", alice, false, "travel"); err != nil {
			t.Fatal(err)
		}
		library, err = db.ListTags(ctx, alice)
		if err != nil {
			t.Fatal(err)
		}
		counts = libraryCounts(library)
		if counts["Voyage"] != 1 || counts["travel"] != 0 {
			t.Fatalf("create+unassign counts = %+v, want Voyage 1 and travel 0", counts)
		}
		if _, ok := counts["travel"]; !ok {
			t.Fatalf("zero-count tag dropped from library: %+v", library)
		}
	})
}

func TestTagAssignmentPermissionsAndAdminNamespace(t *testing.T) {
	eachStore(t, mysqlTestLane, func(t *testing.T, db *Store) {
		ctx := context.Background()
		owner := testUser(t, db, "owner", false)
		other := testUser(t, db, "other", false)
		admin := testUser(t, db, "admin", true)
		testImage(t, db, "owner-image", owner, 1000)
		testImage(t, db, "other-image", other, 2000)
		testImage(t, db, "admin-image", admin, 3000)

		if _, err := db.AddImageTags(ctx, "owner-image", owner, false, []string{"Travel"}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AddImageTags(ctx, "owner-image", other, false, []string{"Travel"}); !errors.Is(err, ErrImageNotFound) {
			t.Fatalf("non-owner add error = %v, want ErrImageNotFound", err)
		}
		if _, err := db.AddImageTags(ctx, "owner-image", other, true, []string{"Travel"}); !errors.Is(err, ErrImageNotFound) {
			t.Fatalf("forged admin add error = %v, want ErrImageNotFound", err)
		}
		otherLibrary, err := db.ListTags(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if len(otherLibrary) != 0 {
			t.Fatalf("unauthorized add created tags: %+v", otherLibrary)
		}

		adminTags, err := db.AddImageTags(ctx, "owner-image", admin, true, []string{"Shared"})
		if err != nil {
			t.Fatal(err)
		}
		if got := imageTagNames(adminTags); len(got) != 2 || got[0] != "Shared" || got[1] != "Travel" {
			t.Fatalf("admin AddImageTags = %v", got)
		}
		ownerLibrary, err := db.ListTags(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		ownerCounts := libraryCounts(ownerLibrary)
		if ownerCounts["Travel"] != 1 || ownerCounts["Shared"] != 1 {
			t.Fatalf("owner library after admin edit = %+v", ownerCounts)
		}
		adminLibrary, err := db.ListTags(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		if len(adminLibrary) != 0 {
			t.Fatalf("admin edit wrote into admin library: %+v", adminLibrary)
		}

		if _, err := db.ReplaceImageTags(ctx, "owner-image", admin, true, []string{"AdminOnly"}); err != nil {
			t.Fatal(err)
		}
		ownerLibrary, err = db.ListTags(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		ownerCounts = libraryCounts(ownerLibrary)
		if ownerCounts["AdminOnly"] != 1 || ownerCounts["Travel"] != 0 || ownerCounts["Shared"] != 0 {
			t.Fatalf("owner library after replace = %+v", ownerCounts)
		}
		ownerView, err := db.ImageTags(ctx, "owner-image", owner, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := imageTagNames(ownerView); len(got) != 1 || got[0] != "AdminOnly" {
			t.Fatalf("owner view after admin replace = %v", got)
		}

		if _, err := db.RemoveImageTag(ctx, "owner-image", admin, true, "AdminOnly"); err != nil {
			t.Fatal(err)
		}
		ownerLibrary, err = db.ListTags(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		if got := libraryCounts(ownerLibrary)["AdminOnly"]; got != 0 {
			t.Fatalf("AdminOnly count after remove = %d, want 0", got)
		}
		if err := db.DeleteTag(ctx, admin, "Travel"); !errors.Is(err, ErrTagNotFound) {
			t.Fatalf("admin deleted owner tag error = %v, want ErrTagNotFound", err)
		}
		if err := db.DeleteTag(ctx, owner, "AdminOnly"); err != nil {
			t.Fatal(err)
		}

		if _, err := db.ReplaceImageTags(ctx, "other-image", other, false, []string{"Temp"}); err != nil {
			t.Fatal(err)
		}
		cleared, err := db.ReplaceImageTags(ctx, "other-image", other, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(cleared) != 0 {
			t.Fatalf("clear replace left tags %v", imageTagNames(cleared))
		}
		otherLibrary, err = db.ListTags(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if got := libraryCounts(otherLibrary)["Temp"]; got != 0 {
			t.Fatalf("Temp count after clear = %d, want 0", got)
		}

		if _, err := db.ImageTags(ctx, "admin-image", owner, false); !errors.Is(err, ErrImageNotFound) {
			t.Fatalf("foreign read error = %v, want ErrImageNotFound", err)
		}
		if _, err := db.AddImageTags(ctx, "admin-image", admin, true, []string{"Mine"}); err != nil {
			t.Fatal(err)
		}
		adminLibrary, err = db.ListTags(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		if got := libraryCounts(adminLibrary)["Mine"]; got != 1 {
			t.Fatalf("admin own library Mine count = %d, want 1", got)
		}
	})
}

func TestTagLimitsAndCascade(t *testing.T) {
	eachStore(t, mysqlTestLane, func(t *testing.T, db *Store) {
		ctx := context.Background()
		user := testUser(t, db, "limits", false)
		testImage(t, db, "limit-image", user, 1000)
		testImage(t, db, "second-image", user, 2000)

		names := make([]string, 0, TagLibraryLimit)
		for i := 0; i < TagLibraryLimit; i++ {
			name := fmt.Sprintf("tag-%03d", i)
			if _, err := db.EnsureTag(ctx, user, name); err != nil {
				t.Fatalf("EnsureTag(%s): %v", name, err)
			}
			names = append(names, name)
		}
		if _, err := db.EnsureTag(ctx, user, "overflow"); !errors.Is(err, ErrTagLimit) {
			t.Fatalf("library overflow error = %v, want ErrTagLimit", err)
		}
		if _, err := db.EnsureTag(ctx, user, names[0]); err != nil {
			t.Fatalf("existing tag at limit: %v", err)
		}

		if _, err := db.AddImageTags(ctx, "limit-image", user, false, names[:ImageTagLimit]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AddImageTags(ctx, "limit-image", user, false, names[ImageTagLimit:ImageTagLimit+1]); !errors.Is(err, ErrImageTagLimit) {
			t.Fatalf("image overflow error = %v, want ErrImageTagLimit", err)
		}
		tags, err := db.ImageTags(ctx, "limit-image", user, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != ImageTagLimit {
			t.Fatalf("image tags after rejected add = %d, want %d", len(tags), ImageTagLimit)
		}
		tooMany := append(append([]string{}, names[:ImageTagLimit]...), names[ImageTagLimit])
		if _, err := db.ReplaceImageTags(ctx, "limit-image", user, false, tooMany); !errors.Is(err, ErrImageTagLimit) {
			t.Fatalf("replace overflow error = %v, want ErrImageTagLimit", err)
		}
		tags, err = db.ImageTags(ctx, "limit-image", user, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != ImageTagLimit {
			t.Fatalf("image tags after rejected replace = %d, want %d", len(tags), ImageTagLimit)
		}
		if _, err := db.AddImageTags(ctx, "limit-image", user, false, []string{names[0], names[0]}); err != nil {
			t.Fatalf("duplicate add at limit: %v", err)
		}

		if _, err := db.AddImageTags(ctx, "second-image", user, false, names[:3]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DeleteImage("limit-image", user, false); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ImageTags(ctx, "limit-image", user, false); !errors.Is(err, ErrImageNotFound) {
			t.Fatalf("ImageTags after image delete error = %v, want ErrImageNotFound", err)
		}
		var attached int
		if err := db.DB.QueryRow("SELECT count(*) FROM image_tags WHERE image_id=?", "limit-image").Scan(&attached); err != nil {
			t.Fatal(err)
		}
		if attached != 0 {
			t.Fatalf("image delete left %d tag assignments", attached)
		}
		library, err := db.ListTags(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		counts := libraryCounts(library)
		for i := 0; i < 3; i++ {
			if counts[names[i]] != 1 {
				t.Fatalf("%s count after image delete = %d, want 1", names[i], counts[names[i]])
			}
		}
		if got, ok := counts[names[3]]; !ok || got != 0 {
			t.Fatalf("%s count after image delete = %d (present %v), want 0 and retained", names[3], got, ok)
		}

		if err := db.DeleteTag(ctx, user, names[0]); err != nil {
			t.Fatal(err)
		}
		tags, err = db.ImageTags(ctx, "second-image", user, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := imageTagNames(tags); len(got) != 2 || got[0] != names[1] || got[1] != names[2] {
			t.Fatalf("second-image tags after DeleteTag = %v", got)
		}
		library, err = db.ListTags(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := libraryCounts(library)[names[0]]; ok {
			t.Fatalf("deleted tag still in library")
		}
	})
}

// mysqlTestLane is the disposable schema suffix provisioned for this suite. The
// fixture account cannot create schemas, so the tag and timeline groups reuse
// the same lane; package tests run sequentially and each deployment is emptied
// on cleanup.
const mysqlTestLane = "other"

// eachStore runs a store test on SQLite and, when the private MySQL lane is
// provisioned, on MySQL. The MySQL lane is skipped when the disposable schema
// is absent so the normal unit-test run does not need a server.
func eachStore(t *testing.T, lane string, run func(t *testing.T, db *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		db, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		run(t, db)
	})
	t.Run("mysql", func(t *testing.T) {
		run(t, openMySQLTestStore(t, lane))
	})
}

func openMySQLTestStore(t *testing.T, lane string) *Store {
	t.Helper()
	file := os.Getenv("PEBRIL_TEST_MYSQL_ENV")
	if file == "" {
		t.Skip("Real MySQL lane not configured; this is not interoperability acceptance")
	}
	values, err := godotenv.Read(file)
	if err != nil {
		t.Fatal("Cannot read private MySQL test configuration")
	}
	c, err := mysql.ParseDSN(values["PEBRIL_MYSQL_DSN"])
	if err != nil || !strings.HasPrefix(c.DBName, "pebril_test_") {
		t.Fatal("Disposable MySQL database required")
	}
	control, err := sql.Open("mysql", c.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	var present int
	err = control.QueryRow("SELECT count(*) FROM information_schema.schemata WHERE schema_name=?", c.DBName+"_"+lane).Scan(&present)
	control.Close()
	if err != nil {
		t.Fatalf("cannot inspect MySQL test lanes: %v", err)
	}
	if present == 0 {
		t.Skipf("MySQL lane schema %s is not provisioned", c.DBName+"_"+lane)
	}
	target := testutil.MySQLDeployment(t, config.Deployment{DBDir: t.TempDir()}, lane)
	db, err := OpenDeployment(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testUser(t *testing.T, db *Store, username string, admin bool) int64 {
	t.Helper()
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota,admin) VALUES(?,x'00',?,?)", username, int64(1<<40), admin)
	if err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

func testImage(t *testing.T, db *Store, id string, userID, createdAt int64) {
	t.Helper()
	policy, err := json.Marshal(config.Snapshot{Revision: 1, Policy: config.Defaults()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.DB.Exec("INSERT INTO images(id,user_id,state,policy,created_at,format,bytes,width,height,frames,error,hash,generation,attempts,next_run,fallback,delivered) VALUES(?,?,'completed',?,?,'',0,0,0,0,'','',0,0,0,0,0)", id, userID, policy, createdAt)
	if err != nil {
		t.Fatalf("insert image %s: %v", id, err)
	}
}

func imageTagNames(tags []ImageTag) []string {
	names := make([]string, 0, len(tags))
	for _, tag := range tags {
		names = append(names, tag.Name)
	}
	return names
}

func libraryCounts(tags []Tag) map[string]int {
	counts := make(map[string]int, len(tags))
	for _, tag := range tags {
		counts[tag.Name] = tag.Count
	}
	return counts
}
