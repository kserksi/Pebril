# Known issues

Assessed on 2026-10-17 against a 0.0.8-dev development preview. Gallery behavior
listed here may not yet be available on the `dev` branch or in published releases.
All six items remain open; this document records problems and acceptance criteria,
not implemented fixes or release dates.

## Priority and order

P2 means a functional feedback or state-handling defect to address first. P3 means
an interaction improvement to address afterward. Order within each group is the
suggested implementation order. These priorities do not imply an authentication
bypass or data-loss finding.

| Order | ID | Priority | Issue | Status |
|---|---|---|---|---|
| 1 | [PEB-001](#peb-001-password-reset-leaves-the-affected-browser-looking-signed-in) | P2 | Password reset leaves stale signed-in UI | Open |
| 2 | [PEB-002](#peb-002-terminal-upload-failure-keeps-an-active-progress-bar) | P2 | Terminal upload failure keeps an active progress bar | Open |
| 3 | [PEB-006](#peb-006-ban-feedback-is-delayed-or-ambiguous) | P2 | Ban feedback is delayed or ambiguous | Open |
| 4 | [PEB-003](#peb-003-quota-editing-requires-manual-byte-arithmetic) | P3 | Quota editing requires manual byte arithmetic | Open |
| 5 | [PEB-004](#peb-004-opening-a-public-result-replaces-the-management-page) | P3 | Opening a public result replaces the management page | Open |
| 6 | [PEB-005](#peb-005-image-details-appear-after-the-entire-gallery) | P3 | Image details appear after the entire gallery | Open |

## PEB-001: Password reset leaves the affected browser looking signed in

- **Trigger:** An administrator resets an ordinary user's password while that user
  has the page open in another browser session.
- **Observed:** The backend revokes the target user's sessions and upload tokens,
  but the browser can retain its signed-in UI, including after a protected request
  returns HTTP 401. An account refresh eventually clears it.
- **Acceptance:** Detect revoked sessions, clear account-bound UI, and require login
  with the new password. Cover an idle page and a protected request after revocation;
  define and verify the idle-page detection delay. Keep the administrator performing
  the reset signed in. Preserve protection against stale responses restoring data.

## PEB-002: Terminal upload failure keeps an active progress bar

- **Trigger:** Upload validation rejects a file, or conversion reaches a terminal
  failure.
- **Observed:** The error and failure count appear, but an indeterminate progress bar
  remains visible and suggests ongoing work.
- **Acceptance:** Hide progress for terminal failures while retaining the error and
  any permitted retry action or temporary link. Keep progress or waiting feedback
  for work still processing or awaiting an automatic retry. A manual retry must
  restore appropriate progress. Failed work must not count as completed. This issue
  does not request higher image limits.

## PEB-006: Ban feedback is delayed or ambiguous

- **Trigger:** An administrator bans a signed-in user, or that user attempts another
  login with the correct password.
- **Observed:** An existing page does not proactively refresh account status. An
  account refresh displays the ban and disables uploads. Login currently maps a
  banned account to the same error as incorrect credentials.
- **Acceptance:** Synchronize the signed-in page and clearly explain the ban. After
  successful password verification, distinguish a banned login from bad credentials;
  retain generic errors for invalid credentials. Cover ban and unban transitions.
  A ban continues to block new logins/uploads while preserving existing sessions,
  previously accepted work, and existing image access.

## PEB-003: Quota editing requires manual byte arithmetic

- **Trigger:** An administrator edits an ordinary user's storage quota.
- **Observed:** The form accepts only raw bytes, such as `1073741824`, with no unit
  selector.
- **Acceptance:** Provide a readable value and selectable capacity units while
  sending exact integer bytes to the existing API. Preserve zero-quota behavior,
  the signed 64-bit bound, and values beyond JavaScript's safe integer range. Merely
  displaying a value or changing its unit must not silently alter the stored quota.
- **Open decision:** B, KiB, MiB, and GiB are proposed. Supported units, fractional
  input, and rounding/rejection rules still require a decision before implementation.

## PEB-004: Opening a public result replaces the management page

- **Trigger:** Select **Open public result** in gallery details.
- **Observed:** The link navigates away from the management page in the current tab.
- **Acceptance:** Open the public result in a new tab with opener isolation, keeping
  the gallery page available. Image serving and access rules remain unchanged.

## PEB-005: Image details appear after the entire gallery

- **Trigger:** Select **View image details** for an item in a gallery page.
- **Observed:** Details appear below the whole list and pagination controls rather
  than directly below the selected row.
- **Acceptance:** Expand details directly beneath the selected item. Preserve paging,
  administrator all-members scope, authorization, stale-response isolation, and
  deletion reconciliation with the upload-result list.

## Assessment evidence and remaining verification

The assessment used owner screenshots, source inspection, and in-memory DOM probes.
The existing account, upload, and gallery DOM suites passed; additional probes
confirmed stale UI after HTTP 401, delayed ban display, terminal-failure progress,
and the gallery link/detail behavior. Banned-login error handling was confirmed by
source inspection, not an authenticated live login reproduction. The quota form
was confirmed from its screenshot and source.

These are assessment results, not regression coverage for fixes. Real-browser
reproduction and applicable regression checks remain required when implementing
and closing each item. No fix is delivered by this document.
