package handler

import (
	"sync"
)

var (
	updateInProgress = map[string]bool{}
	lock             = sync.Mutex{}
)

func AddActivePR(id string) {
	lock.Lock()
	defer lock.Unlock()

	if updateInProgress == nil {
		updateInProgress = map[string]bool{}
	}

	updateInProgress[id] = true
}

func RmoveActivePR(id string) bool {
	lock.Lock()
	defer lock.Unlock()

	if updateInProgress == nil {
		return false
	}

	_, has := updateInProgress[id]
	delete(updateInProgress, id)
	return has
}

func ActivePRPresent() bool {
	lock.Lock()
	defer lock.Unlock()

	return updateInProgress != nil && len(updateInProgress) > 0
}
