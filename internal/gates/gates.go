// Package gates decides whether a commit may be applied.
//
// Every refusal in the system lives here, and nothing here performs I/O: each
// gate is a pure function over state somebody else fetched. That is the whole
// point of the package. In the shell version these decisions were interleaved
// with the `gh` calls that fed them, so the only way to test "an approval on
// an earlier push does not count" was to drive the entire script against stub
// binaries and read a ledger object afterwards. Here it is a function call.
package gates

import (
	"fmt"
	"strings"
	"time"
)

// Protection is main's branch protection, as the forge reports it.
//
// ⚠️ Pointers, not bools. A missing key and an explicit false are different
// facts about a repository, and treating them alike is how this went wrong in
// the shell version: `jq '.allow_force_pushes.enabled // true'` replaced a
// COMPLIANT false with a non-compliant true, because `//` fires on false as
// readily as on null. Absent must be its own case and must never read as
// compliant.
type Protection struct {
	RequiredApprovals           *int
	RequireCodeOwners           *bool
	DismissStaleReviews         *bool
	EnforceAdmins               *bool
	AllowForcePushes            *bool
	AllowDeletions              *bool
	RequireUpToDateBranch       *bool // required_status_checks.strict
	RequireLastPushApproval     *bool // required_pull_request_reviews.require_last_push_approval
	BypassPullRequestAllowances *BypassAllowances
	StatusChecks                []string
}

// BypassAllowances lists the actors GitHub lets skip
// required_approving_review_count and require_code_owner_reviews entirely.
//
// ⚠️ THE ONE FIELD ON THIS TYPE WHERE ABSENT IS THE COMPLIANT VALUE. GitHub
// omits bypass_pull_request_allowances from the wire payload altogether when
// no bypass is configured -- there is no `{}`, only a missing key -- so a nil
// *BypassAllowances means "nobody may bypass" and must NOT be refused the way
// AllowForcePushes' nil is. Refusing it would refuse every correctly
// configured repository, which is exactly backwards.
type BypassAllowances struct {
	Users []string
	Teams []string
	Apps  []string
}

// ProtectionBar is what the applier refuses to run without. Each field of
// Protection above is checked; adding a field here without checking it is the
// failure this type exists to make visible.
func CheckProtection(p Protection, requiredCheck string) []string {
	var problems []string

	if p.RequiredApprovals == nil || *p.RequiredApprovals < 1 {
		problems = append(problems, "required_approving_review_count is below 1")
	}
	if !isTrue(p.RequireCodeOwners) {
		problems = append(problems, "require_code_owner_reviews is off")
	}
	if !isTrue(p.DismissStaleReviews) {
		problems = append(problems, "dismiss_stale_reviews is off")
	}
	if !isTrue(p.EnforceAdmins) {
		problems = append(problems, "enforce_admins is off")
	}
	// The only one whose COMPLIANT value is false, which is exactly why it
	// needs the explicit nil case rather than a truthiness test.
	if p.AllowForcePushes == nil || *p.AllowForcePushes {
		problems = append(problems, "allow_force_pushes is on, or unreadable")
	}
	// Same shape as AllowForcePushes, same reason: docs/design.md lists
	// "Allow force pushes / deletions off" as one requirement, and
	// scripts/protection sets both in the same payload. History is the
	// audit log only if nothing in it can be deleted.
	if p.AllowDeletions == nil || *p.AllowDeletions {
		problems = append(problems, "allow_deletions is on, or unreadable")
	}
	if !isTrue(p.RequireUpToDateBranch) {
		problems = append(problems, "required_status_checks.strict is off: an approval against a stale main can merge")
	}
	// Without this, CheckApproval's "the approval must be at the head sha"
	// (see the doc comment on Approval below) is undercut one level up: a
	// push AFTER the last approval can still merge, because GitHub does not
	// itself require that push to be re-approved. This is that same rule,
	// asked of the branch-protection endpoint instead of the PR.
	if !isTrue(p.RequireLastPushApproval) {
		problems = append(problems, "require_last_push_approval is off: a push after the last approval can still merge")
	}
	// ⚠️ ABSENT IS COMPLIANT HERE, THE ONE PLACE IN THIS FUNCTION WHERE IT
	// IS. GitHub omits bypass_pull_request_allowances entirely when nobody
	// may bypass; a nil pointer is that fact, not a missing read. Only a
	// non-empty list is a problem, and it is named so an operator does not
	// have to go and look: RequiredApprovals and RequireCodeOwners above are
	// not true for whoever is named in it.
	if b := p.BypassPullRequestAllowances; b != nil {
		var who []string
		if len(b.Users) > 0 {
			who = append(who, "users")
		}
		if len(b.Teams) > 0 {
			who = append(who, "teams")
		}
		if len(b.Apps) > 0 {
			who = append(who, "apps")
		}
		if len(who) > 0 {
			problems = append(problems, fmt.Sprintf(
				"bypass_pull_request_allowances is non-empty (%s): required_approving_review_count and require_code_owner_reviews do not apply to them",
				strings.Join(who, ", ")))
		}
	}
	if !contains(p.StatusChecks, requiredCheck) {
		problems = append(problems, fmt.Sprintf("required status check %q is not required", requiredCheck))
	}
	return problems
}

// PullRequest is the merged PR a commit came from, from the DETAIL endpoint.
//
// ⚠️ Merged must come from `repos/{o}/{r}/pulls/{n}`, never from
// `commits/{sha}/pulls`. The list endpoint returns PR objects with no `merged`
// field at all -- only `merged_at` -- so reading `.merged` there is null for
// every PR ever merged. That gate refused the platform's own first merge.
type PullRequest struct {
	Number         int
	Merged         bool
	MergeCommitSHA string
	HeadSHA        string
}

// Review is one review on that PR.
type Review struct {
	User        string
	State       string // APPROVED, CHANGES_REQUESTED, DISMISSED, COMMENTED
	CommitID    string
	SubmittedAt time.Time
}

// Approval decides whether `sha` was approved by `approver`.
//
// ⚠️ The approval must be AT THE HEAD SHA. An approval carried over from an
// earlier push is an approval of code nobody read; GitHub's own
// dismiss_stale_reviews is asked for in Protection, and this checks it again
// rather than trusting that it was on.
//
// A review by the approver that is NOT at the head sha is ordinary API
// noise, not evidence of anything wrong: dismiss_stale_reviews already
// dismisses it on push, and this gate only cares whether a valid approval
// exists at the head. It is silently skipped rather than reported, so a PR
// that got a second push is judged on its current approval, not wedged by
// its history.
func CheckApproval(pr PullRequest, reviews []Review, approver, sha string) []string {
	var problems []string

	if !pr.Merged {
		problems = append(problems, fmt.Sprintf("PR #%d is not merged", pr.Number))
	}
	// ⚠️ Absent must be its own case, same as everywhere else in this file:
	// an empty MergeCommitSHA is not "no opinion", it is "unmerged, or
	// unreadable", and apply.sh:529 refuses it unconditionally.
	if pr.MergeCommitSHA == "" {
		problems = append(problems, fmt.Sprintf("PR #%d has no merge commit recorded", pr.Number))
	} else if pr.MergeCommitSHA != sha {
		problems = append(problems,
			fmt.Sprintf("PR #%d's merge commit is %s, not %s", pr.Number, short(pr.MergeCommitSHA), short(sha)))
	}

	// ⚠️ AN EMPTY HEAD SHA IS REFUSED BEFORE ANY REVIEW IS CONSIDERED, AND
	// IT USED TO FALL THROUGH INTO THE COMPARISON. `r.CommitID !=
	// pr.HeadSHA` is FALSE when both are empty, so a PR with no head sha
	// plus a review carrying no commit id read as APPROVED. Absent is not
	// agreement -- the same rule this file applies to every other absent
	// field, and the one place it was not applied.
	if pr.HeadSHA == "" {
		problems = append(problems, fmt.Sprintf("PR #%d has no head sha recorded", pr.Number))
		return problems
	}

	// ⚠️ THE APPROVER'S LAST WORD AT THE HEAD DECIDES, NOT WHETHER THEY EVER
	// SAID YES. Scanning for any APPROVED counts an approval that was
	// afterwards withdrawn: approve, spot something, request changes on the
	// same push -- and the gate still reads "approved". GitHub's own
	// protection would block that merge, but this gate exists precisely
	// because it does not take the merge's legitimacy on trust.
	//
	// ⚠️ This is a DELIBERATE DIVERGENCE from apply.sh:358, which counts
	// APPROVED reviews and never looks at CHANGES_REQUESTED. Recorded in
	// internal/parity as intended. Raised by the 2026-09-08 security review.
	approved := false
	for _, r := range reviews {
		if r.User != approver || r.CommitID != pr.HeadSHA {
			continue
		}
		switch r.State {
		case "APPROVED":
			approved = true
		case "CHANGES_REQUESTED", "DISMISSED":
			approved = false
		}
		// Every other state -- COMMENTED, PENDING -- says nothing about
		// approval and must not clear one.
	}
	if !approved {
		problems = append(problems,
			fmt.Sprintf("no approval by %s at the PR head %s", approver, short(pr.HeadSHA)))
	}
	return problems
}

// Commit is the merge commit itself, from the forge's commit-detail
// endpoint.
type Commit struct {
	SHA            string
	Verified       *bool
	CommitterLogin *string
}

// CheckMergeCommit refuses a merge commit that the forge did not create
// itself. "Approved by a human, applied by us" only means something if the
// tree at sha is the tree the PR's approval covered, and the only party who
// can vouch for that is GitHub performing its own merge (web-flow) with a
// verified signature (545-556).
//
// ⚠️ Same absent-is-not-compliant shape as Protection and PullRequest: a
// commit whose verification status or committer could not be read is
// refused, never defaulted to true.
func CheckMergeCommit(c Commit) []string {
	var problems []string
	if !isTrue(c.Verified) {
		problems = append(problems, fmt.Sprintf("merge commit %s is not verified", short(c.SHA)))
	}
	committer := "absent"
	if c.CommitterLogin != nil {
		committer = *c.CommitterLogin
	}
	if committer != "web-flow" {
		problems = append(problems,
			fmt.Sprintf("merge commit %s was committed by %s, not github's own web-flow merge -- a merge github did not perform itself is not trustworthy as \"what the approved PR contained\"", short(c.SHA), committer))
	}
	return problems
}

// CheckPlanDigest refuses to apply a plan whose digest does not match the
// one recorded as approved at headSHA. "mine" is the digest of the plan
// about to run, "approved" is what was recorded when the PR was reviewed,
// approvedFound says whether the ledger held the object at all, and key is
// the ledger key it was read from -- named in the refusal because it is the
// thing an operator then goes and looks at.
//
// ⚠️ AN EMPTY APPROVED DIGEST IS "NOBODY REVIEWED IT", NOT "IT DOES NOT
// MATCH", and this used to fall through to the mismatch branch and print
// "(approved , ours <digest>): the world moved between review and apply" --
// a sentence describing a race that did not happen, with a blank where a
// digest should be. apply.sh:462 tests `[ -z "$theirs" ]` for exactly this.
// Both outcomes are a refusal, so nothing was ever unsafe; what was wrong
// was telling the operator the wrong story about why. The comment here
// previously called an empty recorded digest "impossible in practice",
// which is the kind of claim that stops anyone handling it -- the reference
// suite has a test for it. Found by internal/parity, 2026-09-08.
//
// The credentials root is the one exemption (631, and §2.10): CI never
// plans it, so there is never anything to compare against.
func CheckPlanDigest(root, headSHA, key, mine string, approved string, approvedFound bool) []string {
	if root == "credentials" {
		return nil
	}
	var problems []string
	if !approvedFound || approved == "" {
		problems = append(problems,
			fmt.Sprintf("no approved plan recorded for %s at %s (%s): refusing to apply a plan nobody reviewed", root, short(headSHA), key))
		return problems
	}
	if mine != approved {
		problems = append(problems,
			// The key is deliberately NOT named here, only in the
			// unreviewed branch above -- which is where apply.sh:462 names
			// it. A mismatch already prints both digests, and the key is
			// derivable from the root and the sha; adding it would be a
			// divergence in alert text that nobody asked for.
			fmt.Sprintf("the plan for %s does not match the one approved at %s (approved %s, ours %s): the world moved between review and apply", root, short(headSHA), approved, mine))
	}
	return problems
}

func isTrue(b *bool) bool { return b != nil && *b }

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
