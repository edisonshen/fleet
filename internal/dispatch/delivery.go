package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/edisonshen/fleet/internal/state"
)

// DeliveryStatus is the normalized inspection result for a Delivery
// claim. Per DESIGN-dispatch-lifecycle.md the values are `Present` and
// `Absent`. Mirrors how Exclusive controllers return Alive/Dead/Unknown
// in PR2.
type DeliveryStatus string

const (
	DeliveryPresent DeliveryStatus = "Present"
	DeliveryAbsent  DeliveryStatus = "Absent"
)

// DeliveryClaim is the inline Data payload for a delivery-class claim.
//
// PR1 only handles Kind == KindCoordPromptInbox. Path is the on-disk
// resource path; OwnerID is the dispatch that holds the claim; HostID
// and TmuxSocket discriminate same-host different-server cases in PR2.
type DeliveryClaim struct {
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	Path       string     `json:"path"`
	OwnerID    DispatchID `json:"owner_id"`
	HostID     string     `json:"host_id"`
	TmuxSocket string     `json:"tmux_socket,omitempty"`
	Preserve   bool       `json:"preserve,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
}

// Errors surfaced by the Delivery controller. Mapped to the CLI's
// stable outcome enums in cmd/fleet/claims.go.
var (
	// ErrAlreadyAcquired is returned by AcquireAndDeliver when the
	// journal already owns a live claim for (kind, OwnerID). PR1
	// treats this as idempotent success; the CLI maps it to the
	// `already_acquired` outcome (exit 0).
	ErrAlreadyAcquired = errors.New("delivery claim already acquired")

	// ErrAlreadyReleased is returned by Release when the on-disk claim
	// is already in the released state. Maps to `already_released`
	// (exit 0).
	ErrAlreadyReleased = errors.New("delivery claim already released")

	// ErrNotOwned is returned by Release when the on-disk claim's
	// OwnerID does not match the caller's claim. Maps to `not_owned`
	// (exit 10).
	ErrNotOwned = errors.New("delivery claim not owned by caller")

	// ErrAbsent is returned by Inspect when no claim exists. Maps to
	// `absent` (exit 11).
	ErrAbsent = errors.New("delivery claim absent")
)

// CoordPromptInboxPath returns the on-disk path for a
// coord_prompt_inbox claim. Today this is ~/.fleet/inbox/<id>.md —
// same shape the Python skill writes today via
// dispatch.py:write_worker_inbox so the file remains readable by the
// existing SessionStart hook contract during the v0.10 → v0.11
// migration window.
func CoordPromptInboxPath(id DispatchID) (string, error) {
	root, err := state.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "inbox", id.String()+".md"), nil
}

// CoordPromptInboxArchivePath returns ~/.fleet/inbox/archive/<id>.md.
// Used by Release(preserve=true).
func CoordPromptInboxArchivePath(id DispatchID) (string, error) {
	root, err := state.Root()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "inbox", "archive", id.String()+".md"), nil
}

// DeliveryController is the public API surface for delivery-class
// claims.
//
// PR1 ships ONE controller for the coord_prompt_inbox kind. The
// interface shape mirrors DESIGN-dispatch-lifecycle.md §"Delivery
// controller (3 kinds; common shape)" so PR2 can add the other two
// Delivery kinds (handoff_resume_inbox + remote_control_inbox) by
// satisfying the same interface.
type DeliveryController interface {
	// AcquireAndDeliver is the Go-side atomic create+register. The
	// controller writes the journal claim as `allocating` first
	// (tmp+rename of journal), then writes the on-disk resource
	// (the inbox file via state.WriteAtomic), then flips the claim to
	// `live` (another tmp+rename of journal). On any step failure the
	// caller is left with a recoverable state — see the kill-9
	// recovery test (T2) in dispatch_e2e_test.go for the specific
	// failure modes.
	AcquireAndDeliver(j *Journal, claim DeliveryClaim, content io.Reader) error

	// Inspect returns Present|Absent based on the on-disk resource
	// AND the claim's recorded state in the journal. Present requires
	// state == live; a `released` claim returns Absent even if the
	// file lingers on disk.
	Inspect(j *Journal, kind string) (DeliveryStatus, *DeliveryClaim, error)

	// Release tears down the delivery resource. Default semantics
	// (preserve=false) unlink the file; preserve=true renames into the
	// archive subdir. Idempotent — already_released is not an error.
	Release(j *Journal, claim DeliveryClaim) error
}

// NewCoordPromptInboxController returns the PR1 Delivery controller
// for the coord_prompt_inbox kind.
func NewCoordPromptInboxController() DeliveryController {
	return &coordPromptInboxController{}
}

type coordPromptInboxController struct{}

// AcquireAndDeliver writes the inbox file under a journal-managed
// claim. The flow is:
//
//  1. Append claim(state=allocating) to journal, save journal.
//  2. state.WriteAtomic the inbox file.
//  3. Flip claim to state=live in journal, save journal.
//
// At step-1 failure the journal is unchanged. At step-2 failure the
// journal has an `allocating` claim but no file; the sweeper drops
// orphaned allocating claims (PR4) — for PR1 the kill-9 recovery test
// asserts the state shape so PR4's sweeper has a known target. At
// step-3 failure the inbox file exists and is readable, the claim
// is still `allocating`; the sweeper's "file present, claim allocating"
// path flips to live in PR4.
//
// If a live claim already exists for the same dispatch_id + kind
// returns ErrAlreadyAcquired (idempotent success path for the CLI).
func (c *coordPromptInboxController) AcquireAndDeliver(j *Journal, claim DeliveryClaim, content io.Reader) error {
	if claim.Kind != KindCoordPromptInbox {
		return fmt.Errorf("coordPromptInboxController: unsupported kind %q", claim.Kind)
	}
	if claim.Path == "" {
		return errors.New("coordPromptInboxController: claim Path required")
	}
	if claim.OwnerID == "" {
		return errors.New("coordPromptInboxController: claim OwnerID required")
	}
	// Idempotency: a live claim with the same kind is already acquired.
	if i := j.findClaim(claim.Kind); i >= 0 {
		if j.Claims[i].State == ClaimLive {
			return ErrAlreadyAcquired
		}
		// Allocating + released states are recoverable; we re-attempt
		// the full write. The simplest path is to remove the prior
		// entry and re-run.
		j.Claims = append(j.Claims[:i], j.Claims[i+1:]...)
	}

	now := nowFunc().UTC()
	claim.CreatedAt = now
	claim.ReleasedAt = nil

	// Step 1 — append allocating claim, save journal.
	allocData, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("marshal claim: %w", err)
	}
	j.Claims = append(j.Claims, ClaimInline{
		Class: ClassDelivery,
		Kind:  claim.Kind,
		State: ClaimAllocating,
		Data:  allocData,
	})
	if err := SaveJournal(j); err != nil {
		return fmt.Errorf("save journal (allocating): %w", err)
	}

	// Step 2 — write the inbox file. state.WriteAtomic creates parent
	// dirs lazily here as a defensive belt-and-suspenders (Bootstrap
	// usually has them; tests using FLEET_HOME may not).
	if err := os.MkdirAll(filepath.Dir(claim.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir inbox: %w", err)
	}
	body, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("read content: %w", err)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}
	if err := state.WriteAtomic(claim.Path, body); err != nil {
		return fmt.Errorf("write inbox: %w", err)
	}

	// Step 3 — flip claim to live, save journal.
	idx := j.findClaim(claim.Kind)
	if idx < 0 {
		return errors.New("internal: claim disappeared between allocating and live")
	}
	liveData, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("marshal live claim: %w", err)
	}
	j.Claims[idx] = ClaimInline{
		Class: ClassDelivery,
		Kind:  claim.Kind,
		State: ClaimLive,
		Data:  liveData,
	}
	if err := SaveJournal(j); err != nil {
		return fmt.Errorf("save journal (live): %w", err)
	}
	return nil
}

// Inspect returns Present when a live claim for kind exists AND the
// underlying file is on disk; otherwise Absent. The combination keeps
// the controller honest about "claim says live but resource gone"
// (sweeper territory: PR4).
func (c *coordPromptInboxController) Inspect(j *Journal, kind string) (DeliveryStatus, *DeliveryClaim, error) {
	idx := j.findClaim(kind)
	if idx < 0 {
		return DeliveryAbsent, nil, nil
	}
	var claim DeliveryClaim
	if err := json.Unmarshal(j.Claims[idx].Data, &claim); err != nil {
		return DeliveryAbsent, nil, fmt.Errorf("unmarshal claim: %w", err)
	}
	if j.Claims[idx].State != ClaimLive {
		return DeliveryAbsent, &claim, nil
	}
	if _, err := os.Stat(claim.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DeliveryAbsent, &claim, nil
		}
		return DeliveryAbsent, &claim, fmt.Errorf("stat inbox: %w", err)
	}
	return DeliveryPresent, &claim, nil
}

// Release tears down a coord_prompt_inbox delivery. Steps:
//
//  1. If on-disk claim is already `released`, return ErrAlreadyReleased
//     (idempotent success).
//  2. If on-disk claim's OwnerID != caller's claim, return ErrNotOwned.
//  3. Flip claim to `releasing` in journal, save.
//  4. Unlink (preserve=false) or rename to archive (preserve=true) the
//     resource file. Tolerate ENOENT (file already gone is success).
//  5. Flip claim to `released`, save.
//  6. Recompute journal recl_state — when ALL claims are released and
//     exec_state is terminal, set recl_state = complete.
//
// Cross-host check: if the on-disk claim's HostID is set and differs
// from the caller's HostID, returns ErrNotOwned. Same-host is the v0.11
// invariant; cross-host coordination is out of scope (DESIGN §"Non-goals").
func (c *coordPromptInboxController) Release(j *Journal, callerClaim DeliveryClaim) error {
	if callerClaim.Kind != KindCoordPromptInbox {
		return fmt.Errorf("coordPromptInboxController: unsupported kind %q", callerClaim.Kind)
	}
	idx := j.findClaim(callerClaim.Kind)
	if idx < 0 {
		return ErrAlreadyReleased
	}
	if j.Claims[idx].State == ClaimReleased {
		return ErrAlreadyReleased
	}
	var diskClaim DeliveryClaim
	if err := json.Unmarshal(j.Claims[idx].Data, &diskClaim); err != nil {
		return fmt.Errorf("unmarshal claim: %w", err)
	}
	// Ownership check: caller must match the recorded owner. The
	// dispatch_id is the primary discriminator (each dispatch's
	// inbox file is keyed off its own id); HostID is the cross-host
	// guard. Empty caller HostID is tolerated for backward-compat
	// during the PR1 release window — the CLI always supplies it but
	// direct Go callers may not.
	if diskClaim.OwnerID != callerClaim.OwnerID {
		return ErrNotOwned
	}
	if callerClaim.HostID != "" && diskClaim.HostID != "" && diskClaim.HostID != callerClaim.HostID {
		return ErrNotOwned
	}

	// Step 3 — flip to releasing.
	releasingData, err := json.Marshal(diskClaim)
	if err != nil {
		return fmt.Errorf("marshal releasing claim: %w", err)
	}
	j.Claims[idx] = ClaimInline{
		Class: ClassDelivery,
		Kind:  diskClaim.Kind,
		State: ClaimReleasing,
		Data:  releasingData,
	}
	if err := SaveJournal(j); err != nil {
		return fmt.Errorf("save journal (releasing): %w", err)
	}

	// Step 4 — teardown.
	if diskClaim.Preserve {
		arch, perr := CoordPromptInboxArchivePath(diskClaim.OwnerID)
		if perr != nil {
			return fmt.Errorf("resolve archive path: %w", perr)
		}
		if mkerr := os.MkdirAll(filepath.Dir(arch), 0o755); mkerr != nil {
			return fmt.Errorf("mkdir inbox archive: %w", mkerr)
		}
		if rerr := os.Rename(diskClaim.Path, arch); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("archive inbox: %w", rerr)
		}
	} else {
		if rerr := os.Remove(diskClaim.Path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return fmt.Errorf("remove inbox: %w", rerr)
		}
	}

	// Step 5 — flip to released.
	now := nowFunc().UTC()
	diskClaim.ReleasedAt = &now
	releasedData, err := json.Marshal(diskClaim)
	if err != nil {
		return fmt.Errorf("marshal released claim: %w", err)
	}
	j.Claims[idx] = ClaimInline{
		Class: ClassDelivery,
		Kind:  diskClaim.Kind,
		State: ClaimReleased,
		Data:  releasedData,
	}

	// Step 6 — recompute recl_state if all claims released.
	allReleased := true
	for _, c := range j.Claims {
		if c.State != ClaimReleased {
			allReleased = false
			break
		}
	}
	if allReleased && j.ExecState.IsTerminal() {
		j.ReclState = ReclComplete
	}
	if err := SaveJournal(j); err != nil {
		return fmt.Errorf("save journal (released): %w", err)
	}
	return nil
}
