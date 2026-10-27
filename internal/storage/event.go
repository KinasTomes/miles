package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// EventID implements the frozen R2 identity array. Every component is ASCII,
// so JSON encoding of this string array is also its JCS representation.
func EventID(historyID, mutationID, entity, action string) (string, error) {
	if !hex128(historyID) || !hex128(mutationID) {
		return "", fmt.Errorf("%w: event history/mutation identity", ErrInvalid)
	}
	switch entity + "/" + action {
	case "Objects/object_commit", "Chunks/location_set", "SystemConfig/entity_put", "SystemConfig/entity_delete":
	default:
		return "", fmt.Errorf("%w: event entity/action", ErrInvalid)
	}
	body, err := json.Marshal([5]string{"miles-r2-event-v1", historyID, mutationID, entity, action})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func hex128(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
