package skedulr

import (
	"github.com/google/uuid"
)

func generateId() string {
	return uuid.New().String()
}
