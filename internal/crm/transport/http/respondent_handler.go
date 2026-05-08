package http

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	authapi "github.com/sociopulse/platform/internal/auth/api"
	crmapi "github.com/sociopulse/platform/internal/crm/api"
	crmservice "github.com/sociopulse/platform/internal/crm/service"
	authmw "github.com/sociopulse/platform/pkg/middleware/auth"
)

// importMaxBodyBytes is the upper bound on the multipart upload body
// for the import endpoint. 50 MiB per Plan 06 Task 5 spec — should
// comfortably hold a 100k-row XLSX (typical 200-500 bytes/row even
// with attributes, well under the cap with margin for Excel's
// formatting overhead). The service-layer importPayloadInlineLimit is
// the authoritative cap once the body is read; this is the cheap
// "reject before the bytes are buffered" gate.
const importMaxBodyBytes = 50 * 1024 * 1024

// defaultRespondentPageSize is the page size when ?page_size is
// absent. Matches the service default.
const defaultRespondentPageSize = 50

// createRespondent handles POST /api/projects/:id/respondents.
func (h *handlers) createRespondent(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	projectID, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	var req CreateRespondentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		renderBindError(c, err)
		return
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	r, err := h.deps.Respondent.Create(ctx, crmapi.CreateRespondentInput{
		TenantID:   claims.TenantID,
		ProjectID:  projectID,
		Phone:      req.Phone,
		RegionCode: req.RegionCode,
		Attributes: req.Attributes,
		Source:     req.Source,
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusCreated, respondentToDTO(*r))
}

// getRespondent handles GET /api/respondents/:id (operator+).
// Returns the masked-phone projection — the plaintext phone is never
// exposed by this path; admins use /respondents/:id/with-phone.
func (h *handlers) getRespondent(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		renderBindError(c, err)
		return
	}
	r, perr := h.deps.Respondent.Get(c.Request.Context(), id)
	if perr != nil {
		renderError(c, h.deps.Logger, perr)
		return
	}
	c.JSON(http.StatusOK, respondentToDTO(*r))
}

// getRespondentWithPhone handles GET /api/respondents/:id/with-phone
// (admin). The service layer audits the access; we additionally run a
// defence-in-depth RBAC check via the deps.RBAC matrix because the
// HTTP-level requireAdminRole middleware only confirms the role
// claim, not the matrix verb.
func (h *handlers) getRespondentWithPhone(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	id, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	// Matrix-level guard. The matrix today doesn't have a dedicated
	// PII action; admins pass the user.list gate (the same fall-back
	// the auth handlers use for admin verbs). When the matrix gains
	// crm.respondent.read_pii in a future plan, replace ActionUserList
	// here.
	if cerr := h.deps.RBAC.Check(c.Request.Context(), claims, authapi.ActionUserList, authapi.ResourceTenantWide("respondent")); cerr != nil {
		renderError(c, h.deps.Logger, cerr)
		return
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	r, err := h.deps.Respondent.GetWithPhone(ctx, id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, respondentToDTO(*r))
}

// searchRespondents handles GET /api/projects/:id/respondents.
func (h *handlers) searchRespondents(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	projectID, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	page := parseQueryInt(c.Query("page"), 1)
	pageSize := parseQueryInt(c.Query("page_size"), defaultRespondentPageSize)

	var statusPtr *crmapi.RespondentStatus
	if s := c.Query("status"); s != "" {
		rs := crmapi.RespondentStatus(s)
		statusPtr = &rs
	}

	res, err := h.deps.Respondent.Search(c.Request.Context(), crmapi.SearchRespondentsFilter{
		TenantID:    claims.TenantID,
		ProjectID:   projectID,
		Status:      statusPtr,
		PhoneSearch: c.Query("phone"),
		Region:      c.Query("region"),
		Query:       c.Query("query"),
		Page:        page,
		PageSize:    pageSize,
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, SearchRespondentsResponse{
		Respondents: respondentsToDTO(res.Items),
		TotalCount:  res.TotalCount,
		Page:        page,
		PageSize:    pageSize,
	})
}

// deleteRespondent handles DELETE /api/respondents/:id (admin).
// Soft-delete: stamps deleted_at + schedules the 30-day purge.
func (h *handlers) deleteRespondent(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	id, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}
	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	dr, err := h.deps.Respondent.Delete(ctx, id)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, DeletionReceiptDTO{
		RespondentID:     dr.RespondentID.String(),
		ScheduledPurgeAt: dr.DeleteAt,
	})
}

// importRespondents handles POST /api/projects/:id/respondents/import
// (admin). The endpoint accepts both JSON-with-base64-body (operators
// driving via API client) and multipart upload (operators uploading
// via the dashboard).
func (h *handlers) importRespondents(c *gin.Context) {
	claims, ok := authmw.ClaimsFromContext(c)
	if !ok {
		renderError(c, h.deps.Logger, authapi.ErrTokenInvalid)
		return
	}
	projectID, perr := uuid.Parse(c.Param("id"))
	if perr != nil {
		renderBindError(c, perr)
		return
	}

	// Cap the request body so a misbehaving client cannot exhaust
	// memory before validation runs. Matches the service-layer cap.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, importMaxBodyBytes)

	contentType := c.GetHeader("Content-Type")
	var (
		body     []byte
		filename string
		format   crmapi.ImportFormat
	)

	if contentType != "" && len(contentType) >= len("multipart/") && contentType[:len("multipart/")] == "multipart/" {
		fileHeader, ferr := c.FormFile("file")
		if ferr != nil {
			renderBindError(c, ferr)
			return
		}
		f, oerr := fileHeader.Open()
		if oerr != nil {
			renderBindError(c, oerr)
			return
		}
		defer func() { _ = f.Close() }()
		raw, rerr := io.ReadAll(f)
		if rerr != nil {
			renderBindError(c, rerr)
			return
		}
		body = raw
		filename = fileHeader.Filename
		format = inferImportFormat(c.PostForm("format"), fileHeader.Header.Get("Content-Type"), filename)
	} else {
		// JSON body — we accept it as raw bytes via the request body.
		raw, rerr := io.ReadAll(c.Request.Body)
		if rerr != nil {
			renderBindError(c, rerr)
			return
		}
		body = raw
		filename = c.Query("filename")
		format = inferImportFormat(c.Query("format"), contentType, filename)
	}

	ctx := crmservice.WithActorID(c.Request.Context(), claims.UserID)
	ticket, err := h.deps.Respondent.Import(ctx, crmapi.ImportRequest{
		TenantID:    claims.TenantID,
		ProjectID:   projectID,
		Format:      format,
		Filename:    filename,
		ContentType: contentType,
		Body:        body,
	})
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusAccepted, importTicketToDTO(*ticket))
}

// getImportStatus handles GET /api/imports/:job_id (admin).
func (h *handlers) getImportStatus(c *gin.Context) {
	jobID := c.Param("job_id")
	if jobID == "" {
		renderError(c, h.deps.Logger, crmapi.ErrInvalidArgument)
		return
	}
	st, err := h.deps.Respondent.GetImportStatus(c.Request.Context(), jobID)
	if err != nil {
		renderError(c, h.deps.Logger, err)
		return
	}
	c.JSON(http.StatusOK, importStatusToDTO(*st))
}

// inferImportFormat picks csv / xlsx from the explicit form field, the
// Content-Type, or the filename extension. Falls back to ImportFormatCSV
// — the service layer rejects an empty/unknown format with
// ErrImportFormatUnsupported, which renders as a clean 400.
func inferImportFormat(explicit, contentType, filename string) crmapi.ImportFormat {
	if explicit != "" {
		switch explicit {
		case "csv":
			return crmapi.ImportFormatCSV
		case "xlsx":
			return crmapi.ImportFormatXLSX
		}
	}
	switch contentType {
	case "text/csv", "application/csv", "application/vnd.ms-excel":
		return crmapi.ImportFormatCSV
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return crmapi.ImportFormatXLSX
	}
	if len(filename) >= 4 {
		tail := filename[len(filename)-4:]
		switch tail {
		case ".csv", ".CSV":
			return crmapi.ImportFormatCSV
		case "xlsx", "XLSX":
			return crmapi.ImportFormatXLSX
		}
	}
	if len(filename) >= 5 {
		tail := filename[len(filename)-5:]
		switch tail {
		case ".xlsx", ".XLSX":
			return crmapi.ImportFormatXLSX
		}
	}
	return crmapi.ImportFormatCSV
}
