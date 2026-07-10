package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/edisonshen/fleet/internal/agent"
	"github.com/edisonshen/fleet/internal/coordlock"
	"github.com/edisonshen/fleet/internal/coordreconcile"
	"github.com/edisonshen/fleet/internal/projectlookup"
	"github.com/edisonshen/fleet/internal/state"
	"github.com/edisonshen/fleet/internal/tmux"
)

const (
	defaultTUIResolveWaitInterval = time.Second
	defaultTUIResolveMaxAttempts  = 30
)

var (
	tuiCoordNewAgentIDFn = agent.NewID
	tuiCoordResolveFn    = func(project, agentID string) (coordreconcile.Verdict, error) {
		return coordreconcile.Resolve(coordreconcile.DefaultDeps(), project, agentID)
	}
	tuiCoordCurrentOwnerFn      = coordlock.CurrentOwner
	tuiCoordResolveWaitTickFn   = tea.Tick
	tuiCoordResolveWaitInterval = defaultTUIResolveWaitInterval
	tuiCoordResolveMaxAttempts  = defaultTUIResolveMaxAttempts
	tuiKillTmuxSessionFn        = tmux.Kill
)

type coordResolveRetryMsg struct {
	projectName  string
	agentID      string
	context      string
	attemptsLeft int
}

func coordTargetProject(rec *agent.Record) (string, bool) {
	if rec == nil || rec.Project == "" {
		return "", false
	}
	if rec.IsCoord || rec.TaskID == coordTaskID(rec.Project) {
		return rec.Project, true
	}
	return "", false
}

func (m Model) beginResolvedCoordAttach(projectName, context string) (Model, tea.Cmd) {
	if projectName == "" {
		m.flash = &flashMsg{text: "[a] coord attach needs a project name", isErr: true}
		return m, nil
	}
	if op, ok := m.inFlightOp(projectName); ok {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"coord %s for project %s is in flight — wait a moment then re-press [a]",
				coordOpVerb(op), projectName),
			isErr: true,
		}
		return m, nil
	}
	agentID := tuiCoordNewAgentIDFn()
	if agentID == "" {
		m.flash = &flashMsg{
			text:  fmt.Sprintf("project %s: could not preallocate coord id — press [a] again", projectName),
			isErr: true,
		}
		return m, nil
	}
	return m.consumeResolvedCoord(projectName, agentID, context, tuiCoordResolveMaxAttempts)
}

func (m Model) consumeResolvedCoord(projectName, agentID, context string, attemptsLeft int) (Model, tea.Cmd) {
	verdict, err := tuiCoordResolveFn(projectName, agentID)
	if err != nil {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"project %s: cannot resolve coord lease (%v) — press [a] again; if it persists run `fleet doctor`",
				projectName, err),
			isErr: true,
		}
		return m, loadAgentsCmd()
	}
	return m.consumeResolvedCoordVerdict(projectName, agentID, context, attemptsLeft, verdict)
}

func (m Model) consumeResolvedCoordVerdict(projectName, agentID, context string, attemptsLeft int, verdict coordreconcile.Verdict) (Model, tea.Cmd) {
	switch verdict.Decision {
	case coordreconcile.Attach:
		return m.attachResolvedCoordOwner(projectName, context, verdict)
	case coordreconcile.Wait:
		if attemptsLeft <= 0 {
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"project %s: coord is still waiting after bounded lease retries (%s) — press [a] again; check `fleet status` / FLEET_TMUX_SOCKET if it persists",
					projectName, verdict.Reason),
				isErr: true,
			}
			return m, loadAgentsCmd()
		}
		return m, tuiCoordResolveWaitTickFn(tuiCoordResolveWaitInterval, func(time.Time) tea.Msg {
			return coordResolveRetryMsg{
				projectName:  projectName,
				agentID:      agentID,
				context:      context,
				attemptsLeft: attemptsLeft - 1,
			}
		})
	case coordreconcile.Spawn:
		return m.startResolvedCoordSpawn(projectName)
	default:
		m.flash = &flashMsg{
			text:  fmt.Sprintf("project %s: coord resolver returned unknown decision %s — press [a] again", projectName, verdict.Decision),
			isErr: true,
		}
		return m, nil
	}
}

func (m Model) attachResolvedCoordOwner(projectName, context string, verdict coordreconcile.Verdict) (Model, tea.Cmd) {
	ownerID := verdict.Owner.AgentID
	if ownerID == "" {
		m.flash = &flashMsg{
			text:  fmt.Sprintf("project %s: coord lease resolved an empty owner — press [a] again", projectName),
			isErr: true,
		}
		return m, loadAgentsCmd()
	}
	rec := findRecordByID(m.records, ownerID)
	if rec == nil {
		var err error
		rec, err = agent.Load(ownerID)
		if err != nil {
			m.flash = &flashMsg{
				text: fmt.Sprintf(
					"project %s: coord lease is held by %s but its agent record is not readable (%v). Do not respawn beside a process-live owner; check `fleet status` / FLEET_TMUX_SOCKET.",
					projectName, ownerID, err),
				isErr: true,
			}
			return m, loadAgentsCmd()
		}
	}
	session := rec.TmuxSession
	if session == "" {
		session = tmux.SessionName(rec.ID)
	}
	alive, probeErr := sessionProbeFn(session)
	if probeErr != nil || !alive {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"project %s: coord %s is process-live in the lease but session %s is unreachable. Do not respawn beside it; check FLEET_TMUX_SOCKET / `fleet status`, then press [a] again.",
				projectName, ownerID, session),
			isErr: true,
		}
		return m, loadAgentsCmd()
	}
	if _, ok := tuiCoordCurrentOwnerFn(projectName); !ok {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"warning: coord %s for project %s is process-live but lease heartbeat is stale; attaching anyway. If it is hung, run `fleet doctor`.",
				ownerID, projectName),
		}
	} else {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"attached to current coord %s for %s",
				ownerID, projectName),
		}
	}
	if context == "worker" {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"viewing coord chat for project %s (worker renders as a local agent there)",
				projectName),
		}
	}
	m.pendingAttach = session
	return m, tea.Quit
}

// startResolvedCoordSpawn dispatches the actual spawn AFTER a Spawn verdict
// (PR-2 D4: the SpawnStandby verdict is gone — acquiring the flock IS becoming
// the coordinator, so every fresh coord is spawned the same way and races for
// the free flock). The resolver already ran the anti-double-spawn gate; every
// early-return branch below leaves the temporary starting claim to self-heal
// (coordreconcile: a pre-spawn claim, pid unset, becomes claimable again once
// its TTL elapses). Every failure flash must say so and return loadAgentsCmd()
// (never nil) so the dashboard refresh path stays consistent (codex 265b iter-1
// [P1]).
func (m Model) startResolvedCoordSpawn(projectName string) (Model, tea.Cmd) {
	if killedID, err := tuiKillCollidingOrphanTmux(projectName); err != nil {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"project %s: lease resolved spawn but orphan cleanup failed: %v — press [a] again; the temporary starting claim will self-heal after its TTL",
				projectName, err),
			isErr: true,
		}
		return m, loadAgentsCmd()
	} else if killedID != "" {
		m.flash = &flashMsg{
			text: fmt.Sprintf("reaped stale %s (live orphan session) before starting coord for %s", killedID, projectName),
		}
	}
	if _, err := state.EnsureProjectInitialized(projectName); err != nil {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"project %s: init failed: %v — the temporary starting claim will self-heal after its TTL; fix and press [a] again",
				projectName, err),
			isErr: true,
		}
		return m, loadAgentsCmd()
	}
	cwd, rerr := coordRepoForProject(projectName)
	if rerr != nil {
		m.flash = &flashMsg{
			text: fmt.Sprintf(
				"%s — the temporary starting claim will self-heal after its TTL; fix and press [a] again",
				rerr.Error()),
			isErr: true,
		}
		return m, loadAgentsCmd()
	}
	m.setOpInFlight(projectName, coordOpSpawn)
	if m.flash == nil {
		m.flash = &flashMsg{text: fmt.Sprintf("starting coord for project %s via lease resolve", projectName)}
	}
	return m, m.startCoordSpawn(projectName, cwd)
}

func tuiKillCollidingOrphanTmux(projectName string) (string, error) {
	records, badIDs, err := agent.ListStrict()
	if err != nil {
		return "", fmt.Errorf("agent.ListStrict failed: %w", err)
	}
	if len(badIDs) > 0 {
		return "", fmt.Errorf("%d unparseable agent record(s) %v; refusing orphan cleanup before spawn", len(badIDs), badIDs)
	}
	id, ok := projectlookup.OrphanTmuxForProject(records, projectName)
	if !ok {
		return "", nil
	}
	session := tmux.SessionName(id)
	if err := tuiKillTmuxSessionFn(session); err != nil {
		return "", fmt.Errorf("tmux kill-session %s failed: %w; re-run `tmux kill-session -t %s` manually then retry [a]", session, err, session)
	}
	return id, nil
}

func resolveCoordSpawnVeto(projectName string) coordSpawnDoneMsg {
	verdict, err := tuiCoordResolveFn(projectName, "")
	if err != nil {
		return coordSpawnDoneMsg{
			projectName: projectName,
			recoverable: fmt.Sprintf(
				"%s — note: lease re-resolve failed (%v)",
				coordVetoRetryFlash(projectName), err),
		}
	}
	if verdict.Decision != coordreconcile.Attach {
		return coordSpawnDoneMsg{
			projectName: projectName,
			recoverable: fmt.Sprintf(
				"%s — note: lease re-resolve returned %s (%s)",
				coordVetoRetryFlash(projectName), verdict.Decision, verdict.Reason),
		}
	}
	ownerID := verdict.Owner.AgentID
	rec, err := agent.Load(ownerID)
	if err != nil {
		return coordSpawnDoneMsg{
			projectName: projectName,
			recoverable: fmt.Sprintf(
				"%s — note: lease owner %s record is not readable (%v)",
				coordVetoRetryFlash(projectName), ownerID, err),
		}
	}
	session := rec.TmuxSession
	if session == "" {
		session = tmux.SessionName(rec.ID)
	}
	return coordSpawnDoneMsg{
		projectName:      projectName,
		agentID:          ownerID,
		session:          session,
		attachedExisting: true,
	}
}
