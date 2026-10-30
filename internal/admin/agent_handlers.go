package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/integration"
)

type integrationStats struct {
	Calls      int64   `json:"calls_24h"`
	CostUSD    float64 `json:"cost_usd_24h"`
	AvgLatency float64 `json:"avg_latency_ms_24h"`
}

type integrationView struct {
	control.Integration
	StateLabel string           `json:"state_label"`
	Stats      integrationStats `json:"stats"`
}

func (s *Server) integrationsReady(w http.ResponseWriter) *integration.Service {
	if s.Integrations == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "agent integrations are not running")
		return nil
	}
	return s.Integrations
}

func (s *Server) agentCatalog(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	ok(w, svc.Agents(r.Context()))
}

func (s *Server) compatibleProviders(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	agents := svc.Agents(r.Context())
	if len(agents) == 0 {
		fail(w, fmt.Errorf("%w: agent %q", control.ErrNotFound, r.PathValue("agent")))
		return
	}
	out, err := svc.CompatibleProviders(r.Context(), r.PathValue("agent"))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, out)
}

func (s *Server) createIntegration(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	var body integration.CreateRequest
	if !decode(w, r, &body) {
		return
	}
	out, err := svc.Create(s.uiCtx(r), body)
	if errors.Is(err, integration.ErrUnsupported) {
		writeErr(w, http.StatusBadRequest, "unsupported", err.Error())
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	s.Logs.Add("info", "agent integration planned", "agent", body.AgentType, "provider", body.ProviderID)
	ok(w, out)
}

func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	rows, err := svc.DB.ListIntegrations(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]integrationView, 0, len(rows))
	for _, row := range rows {
		out = append(out, s.integrationView(r.Context(), svc, row))
	}
	ok(w, out)
}

func (s *Server) getIntegration(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	row, err := svc.DB.GetIntegration(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, s.integrationView(r.Context(), svc, row))
}

func (s *Server) applyIntegration(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	var body integration.ApplyRequest
	if !decode(w, r, &body) {
		return
	}
	out, err := svc.Apply(s.uiCtx(r), r.PathValue("id"), body)
	if err != nil {
		fail(w, err)
		return
	}
	s.Logs.Add("info", "agent integration applied", "integration", r.PathValue("id"), "state", out.State)
	ok(w, out)
}

func (s *Server) verifyIntegration(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	if !decodeOptional(w, r, &struct{}{}) {
		return
	}
	out, err := svc.Verify(s.uiCtx(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, out)
}

func (s *Server) rotateIntegrationToken(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	if !decodeOptional(w, r, &struct{}{}) {
		return
	}
	token, err := svc.Rotate(s.uiCtx(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	s.Logs.Add("warn", "agent gateway token rotated", "integration", r.PathValue("id"))
	ok(w, map[string]string{"token": token})
}

func (s *Server) rollbackIntegration(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	if !decodeOptional(w, r, &struct{}{}) {
		return
	}
	out, err := svc.Rollback(s.uiCtx(r), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, out)
}

func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	var body struct {
		Rollback bool `json:"rollback"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	out, err := svc.Disconnect(s.uiCtx(r), r.PathValue("id"), body.Rollback)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, out)
}

func (s *Server) integrationActivity(w http.ResponseWriter, r *http.Request) {
	svc := s.integrationsReady(w)
	if svc == nil {
		return
	}
	row, err := svc.DB.GetIntegration(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, s.integrationView(r.Context(), svc, row))
}

func (s *Server) integrationView(ctx context.Context, svc *integration.Service, row control.Integration) integrationView {
	v := integrationView{Integration: row, StateLabel: "Not verified yet"}
	if row.Revoked() {
		v.StateLabel = "Disconnected"
	} else if row.FirstSeenAtMS != nil {
		v.StateLabel = "Verified (traffic seen)"
	} else if row.State == "needs_attention" {
		v.StateLabel = "Needs attention"
	} else if row.State == "applied" {
		v.StateLabel = "Applied - not verified yet"
	}
	if svc.Store == nil {
		return v
	}
	from := time.Now().Add(-24 * time.Hour).UnixMilli()
	_ = svc.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(cost_usd),0),
		COALESCE(AVG(latency_ms),0) FROM calls WHERE integration_id=? AND ts_ms>=?`,
		row.ID, from).Scan(&v.Stats.Calls, &v.Stats.CostUSD, &v.Stats.AvgLatency)
	return v
}
