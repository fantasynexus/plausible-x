package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type ensureGoalRequest struct {
	Domain    string   `json:"domain"`
	EventName string   `json:"event_name"`
	Props     []string `json:"props"`
}

type ensureGoalResponse struct {
	Status string `json:"status"`
}

type server struct {
	db *sql.DB
}

func main() {
	databaseURL := os.Getenv("DATABASE_URL")

	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	srv := &server{db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", srv.health)
	mux.HandleFunc("PUT /ensure-goal", srv.ensureGoal)

	log.Printf("plausible-provisioner listening on :%s", port)

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) ensureGoal(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	startedAt := time.Now()
	var payload ensureGoalRequest

	log.Printf(
		"ensure goal request received: method=%s path=%s remote=%s user_agent=%q",
		r.Method,
		r.URL.Path,
		r.RemoteAddr,
		r.UserAgent(),
	)

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Printf("ensure goal invalid json: remote=%s error=%v", r.RemoteAddr, err)
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	log.Printf(
		"ensure goal payload received: domain=%q event=%q props=%v",
		payload.Domain,
		payload.EventName,
		payload.Props,
	)

	payload.Domain = strings.TrimSpace(payload.Domain)
	payload.EventName = strings.TrimSpace(payload.EventName)
	payload.Props = normalizeProps(payload.Props)

	log.Printf(
		"ensure goal payload normalized: domain=%q event=%q props=%v",
		payload.Domain,
		payload.EventName,
		payload.Props,
	)

	if payload.Domain == "" || payload.EventName == "" {
		log.Printf(
			"ensure goal missing required fields: domain_present=%t event_present=%t",
			payload.Domain != "",
			payload.EventName != "",
		)
		http.Error(w, "domain and event_name are required", http.StatusBadRequest)
		return
	}

	log.Printf("ensure goal resolving site: domain=%s", payload.Domain)
	siteID, err := s.findSiteID(ctx, payload.Domain)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("ensure goal site not found: domain=%s", payload.Domain)
			http.Error(w, "site not found", http.StatusNotFound)
			return
		}

		log.Printf("find site id failed: domain=%s error=%v", payload.Domain, err)
		http.Error(w, "failed to resolve site", http.StatusInternalServerError)
		return
	}

	log.Printf(
		"ensure goal site resolved: domain=%s site_id=%d",
		payload.Domain,
		siteID,
	)
	allowedEventProps, err := s.ensureAllowedEventProps(ctx, siteID, payload.Props)
	if err != nil {
		log.Printf(
			"ensure allowed event props failed: domain=%s site_id=%d props=%v error=%v",
			payload.Domain,
			siteID,
			payload.Props,
			err,
		)
		http.Error(w, "failed to ensure custom properties", http.StatusInternalServerError)
		return
	}

	if len(payload.Props) > 0 {
		log.Printf(
			"ensure goal site custom properties updated: domain=%s site_id=%d allowed_event_props=%v",
			payload.Domain,
			siteID,
			allowedEventProps,
		)
	}
	log.Printf(
		"ensure goal inserting goal: site_id=%d domain=%s event=%s props=%v",
		siteID,
		payload.Domain,
		payload.EventName,
		payload.Props,
	)
	created, err := s.insertGoal(ctx, siteID, payload.EventName)
	if err != nil {
		log.Printf("insert goal failed: domain=%s event=%s props=%v error=%v", payload.Domain, payload.EventName, payload.Props, err)
		http.Error(w, "failed to ensure goal", http.StatusInternalServerError)
		return
	}

	status := "exists"
	if created {
		status = "created"
	}

	log.Printf("goal ensured: domain=%s site_id=%d event=%s props=%v status=%s", payload.Domain, siteID, payload.EventName, payload.Props, status)
	log.Printf(
		"ensure goal request completed: domain=%s event=%s status=%s duration_ms=%d",
		payload.Domain,
		payload.EventName,
		status,
		time.Since(startedAt).Milliseconds(),
	)

	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	_ = json.NewEncoder(w).Encode(ensureGoalResponse{Status: status})
}

func (s *server) findSiteID(ctx context.Context, domain string) (int64, error) {
	var siteID int64

	err := s.db.QueryRowContext(
		ctx,
		`SELECT id FROM sites WHERE domain = $1 LIMIT 1`,
		domain,
	).Scan(&siteID)

	if err == nil {
		log.Printf("find site id succeeded: domain=%s site_id=%d", domain, siteID)
	}

	return siteID, err
}

func (s *server) insertGoal(ctx context.Context, siteID int64, eventName string) (bool, error) {
	var goalID int64

	// Plausible CE v3.2.0 stores custom properties on events and goal filters,
	// but does not expose a separate site-level custom props table to provision.
	log.Printf("insert goal query start: site_id=%d event=%s", siteID, eventName)
	err := s.db.QueryRowContext(
		ctx,
		`
		INSERT INTO goals (
			site_id,
			event_name,
			display_name,
			custom_props,
			scroll_threshold,
			inserted_at,
			updated_at
		)
		VALUES ($1, $2, $2, '{}'::jsonb, -1, NOW(), NOW())
		ON CONFLICT (site_id, event_name, custom_props)
		WHERE event_name IS NOT NULL
		DO NOTHING
		RETURNING id
		`,
		siteID,
		eventName,
	).Scan(&goalID)

	if errors.Is(err, sql.ErrNoRows) {
		log.Printf("insert goal skipped due to conflict: site_id=%d event=%s", siteID, eventName)
		return false, nil
	}

	if err != nil {
		log.Printf("insert goal query error: site_id=%d event=%s error=%v", siteID, eventName, err)
		return false, err
	}

	log.Printf("insert goal query created row: site_id=%d event=%s goal_id=%d", siteID, eventName, goalID)
	return true, nil
}

func (s *server) ensureAllowedEventProps(ctx context.Context, siteID int64, props []string) ([]string, error) {
	if len(props) == 0 {
		log.Printf("ensure allowed event props skipped: site_id=%d reason=no_props", siteID)
		return nil, nil
	}

	var allowedEventProps []string

	log.Printf("ensure allowed event props query start: site_id=%d props=%v", siteID, props)
	err := s.db.QueryRowContext(
		ctx,
		`
		UPDATE sites
		SET allowed_event_props = ARRAY(
			SELECT DISTINCT prop
			FROM unnest(
				COALESCE(allowed_event_props, ARRAY[]::varchar[]) || $2::varchar[]
			) AS prop
			WHERE prop IS NOT NULL AND prop <> ''
			ORDER BY prop
		)
		WHERE id = $1
		RETURNING allowed_event_props
		`,
		siteID,
		props,
	).Scan(&allowedEventProps)

	if errors.Is(err, sql.ErrNoRows) {
		log.Printf("ensure allowed event props site not found: site_id=%d", siteID)
		return nil, sql.ErrNoRows
	}

	if err != nil {
		log.Printf("ensure allowed event props query error: site_id=%d props=%v error=%v", siteID, props, err)
		return nil, err
	}

	log.Printf("ensure allowed event props query updated row: site_id=%d allowed_event_props=%v", siteID, allowedEventProps)
	return allowedEventProps, nil
}

func normalizeProps(props []string) []string {
	if len(props) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(props))
	normalized := make([]string, 0, len(props))

	for _, prop := range props {
		prop = strings.TrimSpace(prop)
		if prop == "" {
			continue
		}

		if _, exists := seen[prop]; exists {
			continue
		}

		seen[prop] = struct{}{}
		normalized = append(normalized, prop)
	}

	slices.Sort(normalized)

	return normalized
}
