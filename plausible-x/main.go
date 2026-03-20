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

	var payload ensureGoalRequest

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	payload.Domain = strings.TrimSpace(payload.Domain)
	payload.EventName = strings.TrimSpace(payload.EventName)
	payload.Props = normalizeProps(payload.Props)

	if payload.Domain == "" || payload.EventName == "" {
		http.Error(w, "domain and event_name are required", http.StatusBadRequest)
		return
	}

	siteID, err := s.findSiteID(ctx, payload.Domain)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "site not found", http.StatusNotFound)
			return
		}

		log.Printf("find site id failed: domain=%s error=%v", payload.Domain, err)
		http.Error(w, "failed to resolve site", http.StatusInternalServerError)
		return
	}

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

	return siteID, err
}

func (s *server) insertGoal(ctx context.Context, siteID int64, eventName string) (bool, error) {
	var goalID int64

	// Plausible CE v3.2.0 stores custom properties on events and goal filters,
	// but does not expose a separate site-level custom props table to provision.
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
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return true, nil
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
