package storage

import (
	"database/sql"
	"fmt"
	"github.com/lib/pq"
	"github.com/matrix-org/dendrite/roomserver/types"
)

const eventsSchema = `
-- The events table holds metadata for each event, the actual JSON is stored
-- separately to keep the size of the rows small.
CREATE SEQUENCE IF NOT EXISTS event_nid_seq;
CREATE TABLE IF NOT EXISTS events (
    -- Local numeric ID for the event.
    event_nid BIGINT PRIMARY KEY DEFAULT nextval('event_nid_seq'),
    -- Local numeric ID for the room the event is in.
    -- This is never 0.
    room_nid BIGINT NOT NULL,
    -- Local numeric ID for the type of the event.
    -- This is never 0.
    event_type_nid BIGINT NOT NULL,
    -- Local numeric ID for the state_key of the event
    -- This is 0 if the event is not a state event.
    event_state_key_nid BIGINT NOT NULL,
    -- Local numeric ID for the state at the event.
    -- This is 0 if we don't know the state at the event.
    -- If the state is not 0 then this event is part of the contiguous
    -- part of the event graph
    -- Since many different events can have the same state we store the
    -- state into a separate state table and refer to it by numeric ID.
    state_snapshot_nid bigint NOT NULL DEFAULT 0,
    -- The textual event id.
    -- Used to lookup the numeric ID when processing requests.
    -- Needed for state resolution.
    -- An event may only appear in this table once.
    event_id TEXT NOT NULL CONSTRAINT event_id_unique UNIQUE,
    -- The sha256 reference hash for the event.
    -- Needed for setting reference hashes when sending new events.
    reference_sha256 BYTEA NOT NULL,
    -- A list of numeric IDs for events that can authenticate this event.
    auth_event_nids BIGINT[] NOT NULL
);
`

const insertEventSQL = "" +
	"INSERT INTO events (room_nid, event_type_nid, event_state_key_nid, event_id, reference_sha256, auth_event_nids)" +
	" VALUES ($1, $2, $3, $4, $5, $6)" +
	" ON CONFLICT ON CONSTRAINT event_id_unique" +
	" DO UPDATE SET event_id = $1" +
	" RETURNING event_nid, state_snapshot_nid"

// Bulk lookup of events by string ID.
// Sort by the numeric IDs for event type and state key.
// This means we can use binary search to lookup entries by type and state key.
const bulkSelectStateEventByIDSQL = "" +
	"SELECT event_type_nid, event_state_key_nid, event_nid FROM events" +
	" WHERE event_id = ANY($1)" +
	" ORDER BY event_type_nid, event_state_key_nid ASC"

const bulkSelectStateAtEventByIDSQL = "" +
	"SELECT event_type_nid, event_state_key_nid, event_nid, state_snapshot_nid FROM events" +
	" WHERE event_id = ANY($1)"

const updateEventStateSQL = "" +
	"UPDATE events SET state_snapshot_nid = $2 WHERE event_nid = $1"

type eventStatements struct {
	insertEventStmt                *sql.Stmt
	bulkSelectStateEventByIDStmt   *sql.Stmt
	bulkSelectStateAtEventByIDStmt *sql.Stmt
	updateEventStateStmt           *sql.Stmt
}

func (s *eventStatements) prepare(db *sql.DB) (err error) {
	_, err = db.Exec(eventsSchema)
	if err != nil {
		return
	}
	if s.insertEventStmt, err = db.Prepare(insertEventSQL); err != nil {
		return
	}
	if s.bulkSelectStateEventByIDStmt, err = db.Prepare(bulkSelectStateEventByIDSQL); err != nil {
		return
	}
	if s.bulkSelectStateAtEventByIDStmt, err = db.Prepare(bulkSelectStateAtEventByIDSQL); err != nil {
		return
	}
	if s.updateEventStateStmt, err = db.Prepare(updateEventStateSQL); err != nil {
		return
	}
	return
}

func (s *eventStatements) insertEvent(
	roomNID types.RoomNID, eventTypeNID types.EventTypeNID, eventStateKeyNID types.EventStateKeyNID,
	eventID string,
	referenceSHA256 []byte,
	authEventNIDs []types.EventNID,
) (types.EventNID, types.StateSnapshotNID, error) {
	nids := make([]int64, len(authEventNIDs))
	for i := range authEventNIDs {
		nids[i] = int64(authEventNIDs[i])
	}
	var eventNID int64
	var stateNID int64
	err := s.insertEventStmt.QueryRow(
		int64(roomNID), int64(eventTypeNID), int64(eventStateKeyNID), eventID, referenceSHA256,
		pq.Int64Array(nids),
	).Scan(&eventNID, &stateNID)
	return types.EventNID(eventNID), types.StateSnapshotNID(stateNID), err
}

func (s *eventStatements) bulkSelectStateEventByID(eventIDs []string) ([]types.StateEntry, error) {
	rows, err := s.bulkSelectStateEventByIDStmt.Query(pq.StringArray(eventIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// We know that we will only get as many results as event IDs
	// because of the unique constraint on event IDs.
	// So we can allocate an array of the correct size now.
	// We might get fewer results than IDs so we adjust the length of the slice before returning it.
	results := make([]types.StateEntry, len(eventIDs))
	i := 0
	for ; rows.Next(); i++ {
		result := &results[i]
		if err = rows.Scan(
			&result.EventNID,
			&result.EventTypeNID,
			&result.EventStateKeyNID,
		); err != nil {
			return nil, err
		}
	}
	if i != len(eventIDs) {
		// If there are fewer rows returned than IDs then we were asked to lookup event IDs we don't have.
		// We don't know which ones were missing because we don't return the string IDs in the query.
		// However it should be possible debug this by replaying queries or entries from the input kafka logs.
		// If this turns out to be impossible and we do need the debug information here, it would be better
		// to do it as a separate query rather than slowing down/complicating the common case.
		return nil, fmt.Errorf("storage: state event IDs missing from the database (%d != %d)", i, len(eventIDs))
	}
	return results, err
}

func (s *eventStatements) bulkSelectStateAtEventByID(eventIDs []string) ([]types.StateAtEvent, error) {
	rows, err := s.bulkSelectStateAtEventByIDStmt.Query(pq.StringArray(eventIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]types.StateAtEvent, len(eventIDs))
	i := 0
	for ; rows.Next(); i++ {
		result := &results[i]
		if err = rows.Scan(
			&result.EventNID,
			&result.EventTypeNID,
			&result.EventStateKeyNID,
			&result.BeforeStateSnapshotNID,
		); err != nil {
			return nil, err
		}
		if result.BeforeStateSnapshotNID == 0 {
			return nil, fmt.Errorf("storage: missing state for event NID %d", result.EventNID)
		}
	}
	if i != len(eventIDs) {
		return nil, fmt.Errorf("storage: event IDs missing from the database (%d != %d)", i, len(eventIDs))
	}
	return results, err
}

func (s *eventStatements) updateEventState(eventNID types.EventNID, stateNID types.StateSnapshotNID) error {
	_, err := s.updateEventStateStmt.Exec(int64(eventNID), int64(stateNID))
	return err
}
