package dynamodb

import (
	suite "github.com/KinasTomes/miles/internal/metadata/contracttest"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	"testing"
)

func TestLocalObservationContract(t *testing.T) {
	suite.RunObservation(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore { m, _ := fixtureStore(t, f); return m })
}

func TestLocalObservationCAS(t *testing.T) {
	suite.RunObservationCAS(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore { m, _ := fixtureStore(t, f); return m })
}
