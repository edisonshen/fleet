package gc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edisonshen/fleet/internal/state"
)

// KindOrphanKicked reaps fleet-guard drain-throttle sentinels whose
// queue file has already been consumed.
const KindOrphanKicked Kind = "orphan-kicked"

func reconcileOrphanKicked(r *Report, opts Options, deps Deps) error {
	if deps.ListOrphanKickedMarkers == nil {
		return errors.New("ListOrphanKickedMarkers dep not wired")
	}
	if deps.StatOrphanKickedQueueFile == nil {
		return errors.New("StatOrphanKickedQueueFile dep not wired")
	}
	if deps.RemoveOrphanKickedMarker == nil {
		return errors.New("RemoveOrphanKickedMarker dep not wired")
	}

	markers, err := deps.ListOrphanKickedMarkers()
	if err != nil {
		return err
	}
	for _, marker := range markers {
		queueFile := strings.TrimSuffix(marker, ".kicked")
		if queueFile == marker {
			continue
		}
		if err := deps.StatOrphanKickedQueueFile(queueFile); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat queue file %s: %w", queueFile, err)
		}

		act := Action{
			Kind:   KindOrphanKicked,
			Target: marker,
			Verb:   VerbWouldRemove,
			Reason: fmt.Sprintf("queue file %s absent", queueFile),
		}
		if opts.Apply {
			if err := deps.RemoveOrphanKickedMarker(marker); err != nil {
				act.Reason = fmt.Sprintf("remove failed: %v", err)
			} else {
				act.Verb = VerbRemoved
			}
		}
		r.Actions = append(r.Actions, act)
	}
	return nil
}

func listOrphanKickedMarkersOnDisk() ([]string, error) {
	dir, err := state.QueueDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("readdir queue: %w", err)
	}
	markers := make([]string, 0)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".kicked") {
			continue
		}
		markers = append(markers, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(markers)
	return markers, nil
}

func statOrphanKickedQueueFile(path string) error {
	_, err := os.Stat(path)
	return err
}

func removeOrphanKickedMarker(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
