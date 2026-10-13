// Package id mints aggregate identifiers from the system CSPRNG.
package id

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/marcustut/herald/internal/app"
	"github.com/marcustut/herald/internal/domain"
)

// Random mints identifiers. crypto/rand.Read never returns an error on
// supported platforms.
type Random struct{}

func (Random) NewReportID() domain.ReportID     { return domain.ReportID("rep_" + hex16()) }
func (Random) NewDeliveryID() domain.DeliveryID { return domain.DeliveryID("del_" + hex16()) }

func hex16() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ app.IDGenerator = Random{}
