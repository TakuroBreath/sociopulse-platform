package api

import (
	"fmt"

	"github.com/google/uuid"
)

// NATS subject constants. The surveys module publishes these to inform
// other modules (analytics, dialer) about version changes.
const (
	// SubjectVersionSaved is published when a new version is persisted.
	SubjectVersionSaved = "tenant.<t>.surveys.version.saved"
	// SubjectVersionActivated is published when a version becomes active.
	SubjectVersionActivated = "tenant.<t>.surveys.version.activated"
)

// SubjectVersionSavedFor returns the concrete subject for the
// surveys.version.saved event for the given tenant.
func SubjectVersionSavedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.surveys.version.saved", tenantID)
}

// SubjectVersionActivatedFor returns the concrete subject for the
// surveys.version.activated event for the given tenant.
func SubjectVersionActivatedFor(tenantID uuid.UUID) string {
	return fmt.Sprintf("tenant.%s.surveys.version.activated", tenantID)
}

// VersionSavedEvent is the payload for SubjectVersionSaved.
type VersionSavedEvent struct {
	SurveyID  uuid.UUID `json:"survey_id"`
	VersionID uuid.UUID `json:"version_id"`
	Major     int       `json:"major"`
	Minor     int       `json:"minor"`
}

// VersionActivatedEvent is the payload for SubjectVersionActivated.
type VersionActivatedEvent struct {
	SurveyID  uuid.UUID `json:"survey_id"`
	VersionID uuid.UUID `json:"version_id"`
}
