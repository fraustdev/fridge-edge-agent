package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a lookup by ID finds nothing.
var ErrNotFound = errors.New("fleet: not found")

// Store is the persistence interface for fleet state: events, fridges, and
// alerts. SQLiteStore is the only implementation for this project; a
// Postgres implementation is a documented upgrade path (see SPEC.md).
type Store interface {
	InsertEvent(ctx context.Context, e Event) (Event, error)
	ListEventsForFridge(ctx context.Context, fridgeID string, limit int) ([]Event, error)
	ListRecentEvents(ctx context.Context, limit int) ([]Event, error)

	UpsertFridge(ctx context.Context, f Fridge) error
	GetFridge(ctx context.Context, id string) (Fridge, error)
	ListFridges(ctx context.Context) ([]Fridge, error)

	CreateAlert(ctx context.Context, a Alert) (Alert, error)
	GetAlert(ctx context.Context, id int64) (Alert, error)
	ListAlerts(ctx context.Context, status AlertStatus) ([]Alert, error)
	ListOpenAlertsForFridge(ctx context.Context, fridgeID string) ([]Alert, error)
	UpdateAlertStatus(ctx context.Context, id int64, status AlertStatus, assignedTo string) (Alert, error)

	Close() error
}

// SQLiteStore is a Store backed by SQLite (via the pure-Go modernc.org/sqlite
// driver, so no cgo/build toolchain is required).
type SQLiteStore struct {
	db *sql.DB
}

// OpenSQLiteStore opens (creating if necessary) a SQLite database at path
// and ensures the schema exists. Use ":memory:" for an ephemeral store.
func OpenSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // avoid SQLITE_BUSY under modernc.org/sqlite's single-writer model

	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	fridge_id TEXT NOT NULL,
	slot_id TEXT,
	type TEXT NOT NULL,
	timestamp TEXT NOT NULL,
	payload TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_fridge ON events(fridge_id, timestamp);

CREATE TABLE IF NOT EXISTS fridges (
	id TEXT PRIMARY KEY,
	status TEXT NOT NULL,
	last_event_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS alerts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	fridge_id TEXT NOT NULL,
	slot_id TEXT,
	source_event TEXT NOT NULL,
	severity TEXT NOT NULL,
	status TEXT NOT NULL,
	assigned_to TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_alerts_fridge ON alerts(fridge_id, status);
`
	_, err := s.db.Exec(schema)
	return err
}

func (s *SQLiteStore) InsertEvent(ctx context.Context, e Event) (Event, error) {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal payload: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (fridge_id, slot_id, type, timestamp, payload) VALUES (?, ?, ?, ?, ?)`,
		e.FridgeID, e.SlotID, string(e.Type), e.Timestamp.UTC().Format(time.RFC3339Nano), string(payload))
	if err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, fmt.Errorf("last insert id: %w", err)
	}
	e.ID = id
	return e, nil
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		var slotID sql.NullString
		var typ, ts, payload string
		if err := rows.Scan(&e.ID, &e.FridgeID, &slotID, &typ, &ts, &payload); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		e.SlotID = slotID.String
		e.Type = EventType(typ)
		parsedTS, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("parse event timestamp: %w", err)
		}
		e.Timestamp = parsedTS
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &e.Payload); err != nil {
				return nil, fmt.Errorf("unmarshal payload: %w", err)
			}
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *SQLiteStore) ListEventsForFridge(ctx context.Context, fridgeID string, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, fridge_id, slot_id, type, timestamp, payload FROM events
		 WHERE fridge_id = ? ORDER BY timestamp DESC LIMIT ?`, fridgeID, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	return scanEvents(rows)
}

func (s *SQLiteStore) ListRecentEvents(ctx context.Context, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, fridge_id, slot_id, type, timestamp, payload FROM events
		 ORDER BY timestamp DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	return scanEvents(rows)
}

func (s *SQLiteStore) UpsertFridge(ctx context.Context, f Fridge) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fridges (id, status, last_event_at) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET status = excluded.status, last_event_at = excluded.last_event_at`,
		f.ID, string(f.Status), f.LastEventAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("upsert fridge: %w", err)
	}
	return nil
}

func (s *SQLiteStore) scanFridge(row *sql.Row) (Fridge, error) {
	var f Fridge
	var status, ts string
	if err := row.Scan(&f.ID, &status, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Fridge{}, ErrNotFound
		}
		return Fridge{}, fmt.Errorf("scan fridge: %w", err)
	}
	f.Status = FridgeStatus(status)
	parsedTS, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Fridge{}, fmt.Errorf("parse fridge timestamp: %w", err)
	}
	f.LastEventAt = parsedTS
	return f, nil
}

func (s *SQLiteStore) GetFridge(ctx context.Context, id string) (Fridge, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, status, last_event_at FROM fridges WHERE id = ?`, id)
	f, err := s.scanFridge(row)
	if err != nil {
		return Fridge{}, err
	}
	alerts, err := s.ListOpenAlertsForFridge(ctx, id)
	if err != nil {
		return Fridge{}, err
	}
	for _, a := range alerts {
		f.OpenAlertIDs = append(f.OpenAlertIDs, a.ID)
	}
	return f, nil
}

func (s *SQLiteStore) ListFridges(ctx context.Context) ([]Fridge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, status, last_event_at FROM fridges ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query fridges: %w", err)
	}
	defer rows.Close()

	var fridges []Fridge
	for rows.Next() {
		var f Fridge
		var status, ts string
		if err := rows.Scan(&f.ID, &status, &ts); err != nil {
			return nil, fmt.Errorf("scan fridge: %w", err)
		}
		f.Status = FridgeStatus(status)
		parsedTS, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("parse fridge timestamp: %w", err)
		}
		f.LastEventAt = parsedTS
		fridges = append(fridges, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, f := range fridges {
		alerts, err := s.ListOpenAlertsForFridge(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range alerts {
			fridges[i].OpenAlertIDs = append(fridges[i].OpenAlertIDs, a.ID)
		}
	}
	return fridges, nil
}

func (s *SQLiteStore) CreateAlert(ctx context.Context, a Alert) (Alert, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO alerts (fridge_id, slot_id, source_event, severity, status, assigned_to, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.FridgeID, a.SlotID, string(a.SourceEvent), string(a.Severity), string(a.Status), a.AssignedTo,
		a.CreatedAt.UTC().Format(time.RFC3339Nano), a.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Alert{}, fmt.Errorf("insert alert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Alert{}, fmt.Errorf("last insert id: %w", err)
	}
	a.ID = id
	return a, nil
}

func scanAlert(scanner interface {
	Scan(dest ...any) error
}) (Alert, error) {
	var a Alert
	var slotID, assignedTo sql.NullString
	var sourceEvent, severity, status, createdAt, updatedAt string
	err := scanner.Scan(&a.ID, &a.FridgeID, &slotID, &sourceEvent, &severity, &status, &assignedTo, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Alert{}, ErrNotFound
		}
		return Alert{}, fmt.Errorf("scan alert: %w", err)
	}
	a.SlotID = slotID.String
	a.AssignedTo = assignedTo.String
	a.SourceEvent = EventType(sourceEvent)
	a.Severity = AlertSeverity(severity)
	a.Status = AlertStatus(status)
	a.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Alert{}, fmt.Errorf("parse created_at: %w", err)
	}
	a.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return Alert{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return a, nil
}

func (s *SQLiteStore) GetAlert(ctx context.Context, id int64) (Alert, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, fridge_id, slot_id, source_event, severity, status, assigned_to, created_at, updated_at
		 FROM alerts WHERE id = ?`, id)
	return scanAlert(row)
}

func (s *SQLiteStore) ListAlerts(ctx context.Context, status AlertStatus) ([]Alert, error) {
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, fridge_id, slot_id, source_event, severity, status, assigned_to, created_at, updated_at
			 FROM alerts ORDER BY created_at DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, fridge_id, slot_id, source_event, severity, status, assigned_to, created_at, updated_at
			 FROM alerts WHERE status = ? ORDER BY created_at DESC`, string(status))
	}
	if err != nil {
		return nil, fmt.Errorf("query alerts: %w", err)
	}
	defer rows.Close()

	var alerts []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

func (s *SQLiteStore) ListOpenAlertsForFridge(ctx context.Context, fridgeID string) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, fridge_id, slot_id, source_event, severity, status, assigned_to, created_at, updated_at
		 FROM alerts WHERE fridge_id = ? AND status != ? ORDER BY created_at DESC`,
		fridgeID, string(AlertResolved))
	if err != nil {
		return nil, fmt.Errorf("query open alerts: %w", err)
	}
	defer rows.Close()

	var alerts []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, a)
	}
	return alerts, rows.Err()
}

func (s *SQLiteStore) UpdateAlertStatus(ctx context.Context, id int64, status AlertStatus, assignedTo string) (Alert, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`UPDATE alerts SET status = ?, assigned_to = ?, updated_at = ? WHERE id = ?`,
		string(status), assignedTo, now, id)
	if err != nil {
		return Alert{}, fmt.Errorf("update alert: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Alert{}, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return Alert{}, ErrNotFound
	}
	return s.GetAlert(ctx, id)
}
