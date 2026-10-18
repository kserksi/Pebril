package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kserksi/Pebril/internal/config"
)

func conversionFixture(t *testing.T, policy config.Runtime) (*Store, Image) {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('fixture',x'00',1024)")
	if err != nil {
		t.Fatal(err)
	}
	user, _ := result.LastInsertId()
	id := strings.Repeat("a", 32)
	if err = db.AcceptUpload(id, user, config.Snapshot{Revision: 7, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if err = db.Received(id); err != nil {
		t.Fatal(err)
	}
	image := Image{ID: id, State: "queued", Hash: strings.Repeat("b", 64), Format: "png", Bytes: 42, Width: 2, Height: 2, Frames: 1}
	if err = db.SaveUpload(image); err != nil {
		t.Fatal(err)
	}
	image, err = db.ClaimConversion(id)
	if err != nil {
		t.Fatal(err)
	}
	return db, image
}
func TestConversionChargingAndIndependentRecords(t *testing.T) {
	db, image := conversionFixture(t, config.Defaults())
	original := FileRole(image.Files, "original")
	result := original
	result.Role = "result"
	if err := db.FinishConversion(image, []ImageFile{original, result}); err != nil {
		t.Fatal(err)
	}
	user, _ := db.User(image.UserID)
	if user.Used != 42 {
		t.Fatal("Duplicate file roles were charged twice", user.Used)
	}
	second := image
	second.ID = strings.Repeat("c", 32)
	second.State = "queued"
	if err := db.AcceptUpload(second.ID, image.UserID, image.Policy); err != nil {
		t.Fatal(err)
	}
	if err := db.Received(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveUpload(second); err != nil {
		t.Fatal(err)
	}
	user, _ = db.User(image.UserID)
	if user.Used != 84 {
		t.Fatal("Separate records received a deduplication discount", user.Used)
	}
	if !db.ObjectReferenced(original.Hash) {
		t.Fatal("Retained object was unreferenced")
	}
	stored, err := db.Image(image.ID)
	if err != nil || stored.URL != "/image/"+image.ID || stored.TotalBytes != 42 {
		t.Fatal("Completed receipt is invalid", stored, err)
	}
}
func TestStaleConversionAndAcceptedQuotaOverflow(t *testing.T) {
	db, stale := conversionFixture(t, config.Defaults())
	if err := db.RequeueConversion(stale.ID, stale.Generation); err != nil {
		t.Fatal(err)
	}
	current, err := db.ClaimConversion(stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := FileRole(current.Files, "original")
	output := original
	output.Role = "result"
	output.Hash = strings.Repeat("d", 64)
	output.Bytes = 1000
	stale.Hash = output.Hash
	stale.Bytes = output.Bytes
	if err = db.FinishConversion(stale, []ImageFile{original, output}); err == nil {
		t.Fatal("Stale generation published output")
	}
	user, _ := db.User(stale.UserID)
	if user.Used != 42 {
		t.Fatal("Stale generation changed usage")
	}
	if _, err = db.DB.Exec("UPDATE users SET quota=1 WHERE id=?", current.UserID); err != nil {
		t.Fatal(err)
	}
	current.Hash = output.Hash
	current.Bytes = output.Bytes
	if err = db.FinishConversion(current, []ImageFile{original, output}); err != nil {
		t.Fatal("Accepted work was discarded after quota reduction", err)
	}
	user, _ = db.User(current.UserID)
	if user.Used != 1042 {
		t.Fatal("Result and original charges are incorrect", user.Used)
	}
	if err = db.AcceptUpload("new", current.UserID, current.Policy); !errors.Is(err, ErrQuota) {
		t.Fatal("Over-quota account accepted another upload", err)
	}
}
func TestRetryBudgetFallbackContinuityAndSlots(t *testing.T) {
	db, image := conversionFixture(t, config.Defaults())
	for failed := 1; failed <= 6; failed++ {
		if err := db.ConversionFailed(image, "Conversion failed"); err != nil {
			t.Fatal(err)
		}
		stored, err := db.Image(image.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Attempts != failed || !stored.Temporary || stored.URL != "/image/"+image.ID {
			t.Fatal("Failure lost its stable fallback", stored)
		}
		if failed == 6 {
			if stored.State != "failed" {
				t.Fatal("Retry budget was not exhausted")
			}
			break
		}
		remaining := time.Until(time.UnixMilli(stored.NextRun))
		if stored.State != "retry_wait" || remaining < 29*time.Second || remaining > 31*time.Second {
			t.Fatal("Default retry schedule is incorrect", stored)
		}
		if _, err = db.DB.Exec("UPDATE images SET next_run=0 WHERE id=?", image.ID); err != nil {
			t.Fatal(err)
		}
		image, err = db.ClaimConversion(image.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	for n := range 10 {
		if err := db.AcceptUpload(strings.Repeat("e", 31)+string(rune('a'+n)), image.UserID, image.Policy); err != nil {
			t.Fatal("Exhausted retries kept a task slot", err)
		}
	}
	if err := db.RetryConversion(image.ID, image.UserID); !errors.Is(err, ErrCapacity) {
		t.Fatal("Manual retry bypassed task admission", err)
	}
	if err := db.FailUpload(strings.Repeat("e", 31)+"a", "Rejected"); err != nil {
		t.Fatal(err)
	}
	if err := db.RetryConversion(image.ID, image.UserID); err != nil {
		t.Fatal(err)
	}
	stored, err := db.Image(image.ID)
	if err != nil || stored.State != "queued" || !stored.Temporary || stored.URL != "/image/"+image.ID {
		t.Fatal("Manual retry interrupted fallback access", stored, err)
	}
}
func TestDisabledFallbackAndRetryDeletesFailedInput(t *testing.T) {
	policy := config.Defaults()
	policy.OriginalFallback = false
	policy.AutomaticRetry = false
	db, image := conversionFixture(t, policy)
	if err := db.ConversionFailed(image, "Conversion failed"); err != nil {
		t.Fatal(err)
	}
	stored, err := db.Image(image.ID)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := db.User(image.UserID)
	if stored.State != "failed" || stored.URL != "" || stored.Hash != "" || len(stored.Files) != 0 || user.Used != 0 {
		t.Fatal("Terminal failure retained an input or charge", stored, user)
	}
	if err = db.RetryConversion(image.ID, image.UserID); err == nil {
		t.Fatal("Discarded source remained retryable")
	}
}
func TestInterruptedConversionRecoveryDoesNotConsumeRetries(t *testing.T) {
	db, image := conversionFixture(t, config.Defaults())
	if err := db.RestoreConversions(true); err != nil {
		t.Fatal(err)
	}
	stored, err := db.Image(image.ID)
	if err != nil || stored.State != "queued" || stored.Attempts != 0 || stored.Generation <= image.Generation {
		t.Fatal("Interrupted conversion was not safely restored", stored, err)
	}
	if err = db.FinishConversion(image, image.Files); err == nil {
		t.Fatal("Pre-restart generation published output")
	}
	if err = db.RestoreConversions(false); err != nil {
		t.Fatal(err)
	}
	stored, _ = db.Image(image.ID)
	if stored.State != "failed" || stored.Temporary {
		t.Fatal("Disabled restoration started conversion or exposed original")
	}
}

func TestSchedulingRotatesUsersAndPrioritizesNewImages(t *testing.T) {
	db, first := conversionFixture(t, config.Defaults())
	if err := db.ConversionFailed(first, "Retry"); err != nil {
		t.Fatal(err)
	}
	db.DB.Exec("UPDATE images SET next_run=0 WHERE id=?", first.ID)
	result, err := db.DB.Exec("INSERT INTO users(username,password,quota) VALUES('second-fixture',x'00',1024)")
	if err != nil {
		t.Fatal(err)
	}
	secondUser, _ := result.LastInsertId()
	add := func(id string, user int64) {
		t.Helper()
		if err := db.AcceptUpload(id, user, first.Policy); err != nil {
			t.Fatal(err)
		}
		db.Received(id)
		image := first
		image.ID = id
		image.State = "queued"
		if err := db.SaveUpload(image); err != nil {
			t.Fatal(err)
		}
	}
	own, other := strings.Repeat("c", 32), strings.Repeat("d", 32)
	add(own, first.UserID)
	add(other, secondUser)
	selected, err := db.NextConversion(first.UserID, false)
	if err != nil || selected != other {
		t.Fatal("Users did not rotate", selected, err)
	}
	db.ClaimConversion(other)
	selected, err = db.NextConversion(secondUser, false)
	if err != nil || selected != own {
		t.Fatal("Retry overtook new work", selected, err)
	}
	db.ClaimConversion(own)
	selected, err = db.NextConversion(first.UserID, false)
	if err != nil || selected != first.ID {
		t.Fatal("Eligible retry was not scheduled", selected, err)
	}
}

func TestRestrictedQueueKeepsAVIFWithoutBlockingOtherWork(t *testing.T) {
	policy := config.Defaults()
	policy.StaticFormat = "avif"
	db, avif := conversionFixture(t, policy)
	if err := db.RequeueConversion(avif.ID, avif.Generation); err != nil {
		t.Fatal(err)
	}
	add := func(id, format string, frames int, output string) {
		t.Helper()
		snapshot := avif.Policy
		snapshot.Policy.StaticFormat = output
		if err := db.AcceptUpload(id, avif.UserID, snapshot); err != nil {
			t.Fatal(err)
		}
		if err := db.Received(id); err != nil {
			t.Fatal(err)
		}
		image := Image{ID: id, State: "queued", Hash: strings.Repeat("b", 64), Format: format, Bytes: 42, Width: 2, Height: 2, Frames: frames}
		if err := db.SaveUpload(image); err != nil {
			t.Fatal(err)
		}
	}
	input, plain, animated := strings.Repeat("c", 32), strings.Repeat("d", 32), strings.Repeat("e", 32)
	add(input, "avif", 1, "webp")
	add(plain, "png", 1, "webp")
	add(animated, "gif", 2, "avif")
	for _, expected := range []string{plain, animated} {
		id, err := db.NextConversion(avif.UserID, true)
		if err != nil || id != expected {
			t.Fatal("Restricted queue blocked eligible conversion", id, err)
		}
		if _, err = db.ClaimConversion(id); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := db.NextConversion(avif.UserID, true); err == nil {
		t.Fatal("Restricted mode selected AVIF", id)
	}
	for _, id := range []string{avif.ID, input} {
		image, err := db.Image(id)
		if err != nil || image.State != "queued" || image.Attempts != 0 {
			t.Fatal("Restricted mode changed accepted AVIF task", image, err)
		}
	}
	id, err := db.NextConversion(avif.UserID, false)
	if err != nil || id != avif.ID {
		t.Fatal("Normal startup did not resume AVIF queue", id, err)
	}
}

func TestSiteCapacityAndManualRetryReacquisition(t *testing.T) {
	policy := config.Defaults()
	policy.AutomaticRetry = false
	db, image := conversionFixture(t, policy)
	if err := db.ConversionFailed(image, "Conversion failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec("INSERT INTO users(id,username,password,quota) VALUES(2,'site-fixture',x'00',?)", DefaultQuota); err != nil {
		t.Fatal(err)
	}
	// Seed persisted active states directly; the boundary does not require 10,000 transfers.
	if _, err := db.DB.Exec(`WITH RECURSIVE tasks(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM tasks WHERE n<9999)
INSERT INTO images(id,user_id,state,policy,created_at)
SELECT printf('%032x',n),2,CASE n%5 WHEN 0 THEN 'receiving' WHEN 1 THEN 'received' WHEN 2 THEN 'queued' WHEN 3 THEN 'processing' ELSE 'retry_wait' END,?,n FROM tasks`, "{}"); err != nil {
		t.Fatal(err)
	}
	if err := db.AcceptUpload("site-boundary", image.UserID, image.Policy); err != nil {
		t.Fatal("The 10,000th active task was refused", err)
	}
	if err := db.AcceptUpload("site-overflow", image.UserID, image.Policy); !errors.Is(err, ErrCapacity) {
		t.Fatal("The site admitted more than 10,000 active tasks", err)
	}
	if err := db.RetryConversion(image.ID, image.UserID); !errors.Is(err, ErrCapacity) {
		t.Fatal("Manual retry bypassed the site limit", err)
	}
	if err := db.FailUpload("site-boundary", "Reception failed"); err != nil {
		t.Fatal(err)
	}
	if err := db.RetryConversion(image.ID, image.UserID); err != nil {
		t.Fatal("Manual retry could not reacquire the released site slot", err)
	}
	stored, err := db.Image(image.ID)
	if err != nil || stored.State != "queued" || stored.Attempts != 0 || !stored.Temporary {
		t.Fatal("Manual retry lost its queue, retry budget or fallback", stored, err)
	}
	if err := db.AcceptUpload("site-overflow", image.UserID, image.Policy); !errors.Is(err, ErrCapacity) {
		t.Fatal("Manual retry did not occupy its reacquired site slot", err)
	}
}
