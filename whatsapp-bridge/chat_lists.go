package main

// Chat lists: the filters at the top of WhatsApp's chat list ("Favorites",
// custom lists like "Work"...). WhatsApp syncs them between devices as app
// state *labels*: a LabelEdit mutation per list (name, colour, list type,
// order, deleted) and a LabelAssociationChat mutation per (list, chat). Both
// live in the "regular" app state patch.
//
// The bridge keeps them in messages.db (labels, label_chats) so the MCP
// server can read them without a round trip, and writes go through
// whatsmeow's appstate.BuildLabelChat + SendAppState.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	ErrListNotFound    = errors.New("list not found")
	ErrListNotEditable = errors.New("only custom lists can be edited")
)

// ensureChatListTables creates the tables the list events are stored in.
// Each row keeps the timestamp of the mutation that last changed it, so an
// older mutation replayed out of order (full sync, retries) cannot undo a
// newer one, and seen_at: when an event last mentioned it. whatsmeow only
// dispatches SET mutations, so a list or membership removed on the phone
// never arrives as an event; after a full sync, rows the snapshot did not
// mention are stale (see ReconcileChatLists).
func ensureChatListTables(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS labels (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			color INTEGER,
			list_type TEXT NOT NULL DEFAULT '',
			order_index INTEGER,
			deleted INTEGER NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL,
			seen_at TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS label_chats (
			label_id TEXT NOT NULL,
			chat_jid TEXT NOT NULL,
			labeled INTEGER NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			seen_at TIMESTAMP,
			PRIMARY KEY (label_id, chat_jid)
		);
	`)
	if err != nil {
		return err
	}
	// Databases created by the first version of this file lack seen_at.
	if err := ensureColumn(db, "labels", "seen_at", "TIMESTAMP"); err != nil {
		return err
	}
	return ensureColumn(db, "label_chats", "seen_at", "TIMESTAMP")
}

// StoreLabelEdit applies a LabelEdit mutation unless a newer one is stored.
func (store *MessageStore) StoreLabelEdit(ev *events.LabelEdit) error {
	act := ev.Action
	if act == nil || ev.LabelID == "" {
		return nil
	}
	var color, order sql.NullInt64
	if act.Color != nil {
		color = sql.NullInt64{Int64: int64(act.GetColor()), Valid: true}
	}
	if act.OrderIndex != nil {
		order = sql.NullInt64{Int64: int64(act.GetOrderIndex()), Valid: true}
	}
	listType := ""
	if act.Type != nil {
		listType = act.GetType().String()
	}
	// seen_at always moves; the values only when the mutation is not older.
	_, err := store.db.Exec(`
		INSERT INTO labels (id, name, color, list_type, order_index, deleted, updated_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = CASE WHEN excluded.updated_at >= labels.updated_at THEN excluded.name ELSE labels.name END,
			color = CASE WHEN excluded.updated_at >= labels.updated_at THEN excluded.color ELSE labels.color END,
			list_type = CASE WHEN excluded.updated_at >= labels.updated_at THEN excluded.list_type ELSE labels.list_type END,
			order_index = CASE WHEN excluded.updated_at >= labels.updated_at THEN excluded.order_index ELSE labels.order_index END,
			deleted = CASE WHEN excluded.updated_at >= labels.updated_at THEN excluded.deleted ELSE labels.deleted END,
			updated_at = MAX(excluded.updated_at, labels.updated_at),
			seen_at = excluded.seen_at`,
		ev.LabelID, act.GetName(), color, listType, order, act.GetDeleted(), ev.Timestamp.UTC(), time.Now().UTC())
	return err
}

// StoreLabelChat applies a LabelAssociationChat mutation unless a newer one
// is stored. Removals are kept as labeled=0 rows so their timestamp still
// guards against an older "add" replayed later.
func (store *MessageStore) StoreLabelChat(ev *events.LabelAssociationChat) error {
	if ev.LabelID == "" || ev.JID.IsEmpty() {
		return nil
	}
	labeled := ev.Action != nil && ev.Action.GetLabeled()
	_, err := store.db.Exec(`
		INSERT INTO label_chats (label_id, chat_jid, labeled, updated_at, seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(label_id, chat_jid) DO UPDATE SET
			labeled = CASE WHEN excluded.updated_at >= label_chats.updated_at THEN excluded.labeled ELSE label_chats.labeled END,
			updated_at = MAX(excluded.updated_at, label_chats.updated_at),
			seen_at = excluded.seen_at`,
		ev.LabelID, ev.JID.String(), labeled, ev.Timestamp.UTC(), time.Now().UTC())
	return err
}

// ReconcileChatLists runs after a successful full sync started at `since`:
// lists and memberships the snapshot did not mention were removed on another
// device (as REMOVE mutations, which whatsmeow does not dispatch). As a
// safety net it does nothing when the sync mentioned no list at all.
// Returns the number of lists and memberships it retired.
func (store *MessageStore) ReconcileChatLists(since time.Time) (int64, int64, error) {
	since = since.UTC()
	var seen int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM labels WHERE seen_at >= ?`, since).Scan(&seen); err != nil {
		return 0, 0, err
	}
	if seen == 0 {
		return 0, 0, nil
	}
	lists, err := store.db.Exec(`UPDATE labels SET deleted = 1 WHERE deleted = 0 AND (seen_at IS NULL OR seen_at < ?)`, since)
	if err != nil {
		return 0, 0, err
	}
	members, err := store.db.Exec(`UPDATE label_chats SET labeled = 0 WHERE labeled = 1 AND (seen_at IS NULL OR seen_at < ?)`, since)
	if err != nil {
		return 0, 0, err
	}
	nl, _ := lists.RowsAffected()
	nm, _ := members.RowsAffected()
	return nl, nm, nil
}

// knownChat reports whether a JID is a chat the bridge has seen or one
// already in some list, so a mistyped JID is not written to a list.
func (store *MessageStore) knownChat(jid string) bool {
	var n int
	_ = store.db.QueryRow(`SELECT (SELECT COUNT(*) FROM chats WHERE jid = ?) + (SELECT COUNT(*) FROM label_chats WHERE chat_jid = ?)`, jid, jid).Scan(&n)
	return n > 0
}

// ChatList is one list as returned by GET /api/lists.
type ChatList struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ListType   string `json:"list_type"`
	Color      *int64 `json:"color,omitempty"`
	OrderIndex *int64 `json:"order_index,omitempty"`
	ChatCount  int    `json:"chat_count"`
}

// GetChatLists returns the lists that are not deleted, in WhatsApp's order.
func (store *MessageStore) GetChatLists() ([]ChatList, error) {
	rows, err := store.db.Query(`
		SELECT l.id, l.name, l.list_type, l.color, l.order_index,
			(SELECT COUNT(*) FROM label_chats c WHERE c.label_id = l.id AND c.labeled = 1)
		FROM labels l
		WHERE l.deleted = 0
		ORDER BY l.order_index IS NULL, l.order_index, l.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lists := []ChatList{}
	for rows.Next() {
		var l ChatList
		var color, order sql.NullInt64
		if err := rows.Scan(&l.ID, &l.Name, &l.ListType, &color, &order, &l.ChatCount); err != nil {
			return nil, err
		}
		if color.Valid {
			l.Color = &color.Int64
		}
		if order.Valid {
			l.OrderIndex = &order.Int64
		}
		lists = append(lists, l)
	}
	return lists, rows.Err()
}

// GetChatList returns one list that is not deleted, or ErrListNotFound.
func (store *MessageStore) GetChatList(id string) (ChatList, error) {
	lists, err := store.GetChatLists()
	if err != nil {
		return ChatList{}, err
	}
	for _, l := range lists {
		if l.ID == id {
			return l, nil
		}
	}
	return ChatList{}, ErrListNotFound
}

// ListMemberJIDs returns the chat JIDs (as WhatsApp stores them, usually
// @lid for people and @g.us for groups) currently in a list.
func (store *MessageStore) ListMemberJIDs(listID string) ([]string, error) {
	rows, err := store.db.Query(`SELECT chat_jid FROM label_chats WHERE label_id = ? AND labeled = 1 ORDER BY chat_jid`, listID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jids []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		jids = append(jids, j)
	}
	return jids, rows.Err()
}

// IsInList reports whether chatJID (exact form) is labeled in listID.
func (store *MessageStore) IsInList(listID, chatJID string) (bool, error) {
	var labeled bool
	err := store.db.QueryRow(`SELECT labeled FROM label_chats WHERE label_id = ? AND chat_jid = ?`, listID, chatJID).Scan(&labeled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return labeled, err
}

// chatName returns the stored name of a chat, or "".
func (store *MessageStore) chatName(jid string) string {
	var name sql.NullString
	_ = store.db.QueryRow(`SELECT name FROM chats WHERE jid = ?`, jid).Scan(&name)
	return name.String
}

// handleChatListEvent stores a list event; it reports whether evt was one.
func handleChatListEvent(store *MessageStore, evt interface{}) bool {
	switch v := evt.(type) {
	case *events.LabelEdit:
		if err := store.StoreLabelEdit(v); err != nil {
			fmt.Printf("Failed to store list %s: %v\n", v.LabelID, err)
		} else if !v.FromFullSync {
			fmt.Printf("List %s updated (%q, deleted=%v)\n", v.LabelID, v.Action.GetName(), v.Action.GetDeleted())
		}
		return true
	case *events.LabelAssociationChat:
		if err := store.StoreLabelChat(v); err != nil {
			fmt.Printf("Failed to store list %s membership: %v\n", v.LabelID, err)
		} else if !v.FromFullSync {
			fmt.Printf("Chat %s %s list %s\n", v.JID, map[bool]string{true: "added to", false: "removed from"}[v.Action.GetLabeled()], v.LabelID)
		}
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// chatListDeps is what the list endpoints need from WhatsApp, injectable for
// tests: send an app state patch, resolve a phone/JID to the JID lists use
// (LID for people), and map a LID back to a phone JID for display.
type chatListDeps struct {
	store    *MessageStore
	send     func(ctx context.Context, patch appstate.PatchInfo) error
	resolve  func(chat string) (types.JID, error)
	pnForLID func(ctx context.Context, lid types.JID) (types.JID, error)
	sync     func(ctx context.Context) error
}

func newChatListDeps(client *whatsmeow.Client, store *MessageStore) chatListDeps {
	return chatListDeps{
		store: store,
		send:  client.SendAppState,
		resolve: func(chat string) (types.JID, error) {
			return resolveRecipientJID(client, chat)
		},
		pnForLID: func(ctx context.Context, lid types.JID) (types.JID, error) {
			return client.Store.LIDs.GetPNForLID(ctx, lid)
		},
		sync: func(ctx context.Context) error { return syncChatLists(ctx, client, store) },
	}
}

// ListChat is one chat in a list as returned by GET /api/lists/{id}/chats.
type ListChat struct {
	ListJID string `json:"list_jid"`        // the form WhatsApp stores (@lid, @g.us)
	JID     string `json:"jid"`             // phone JID when known, else list_jid
	Phone   string `json:"phone,omitempty"` // digits, when known
	Name    string `json:"name,omitempty"`  // from the chats table, when known
	IsGroup bool   `json:"is_group"`
}

// listTargetJID turns a phone number or JID into the JID a list membership
// uses: groups as they are; people as their LID, which is how WhatsApp
// stores every person in a list.
func (d chatListDeps) listTargetJID(chat string) (types.JID, error) {
	chat = strings.TrimSpace(chat)
	if chat == "" {
		return types.JID{}, errors.New("chat is required")
	}
	if strings.HasSuffix(chat, "@"+types.GroupServer) || strings.HasSuffix(chat, "@"+types.HiddenUserServer) {
		jid, err := types.ParseJID(chat)
		if err != nil {
			return types.JID{}, fmt.Errorf("invalid JID %q: %w", chat, err)
		}
		known := d.store.knownChat(jid.String())
		if !known && jid.Server == types.HiddenUserServer && d.pnForLID != nil {
			pn, err := d.pnForLID(context.Background(), jid)
			known = err == nil && !pn.IsEmpty()
		}
		if !known {
			return types.JID{}, fmt.Errorf("unknown chat %s: pass a phone number, or a JID of an existing chat", chat)
		}
		return jid, nil
	}
	jid, err := d.resolve(strings.TrimPrefix(chat, "+"))
	if err != nil {
		return types.JID{}, err
	}
	if jid.Server != types.HiddenUserServer {
		return types.JID{}, fmt.Errorf("could not find the WhatsApp ID (LID) of %s; lists store people by LID", chat)
	}
	return jid, nil
}

func (d chatListDeps) members(ctx context.Context, listID string) ([]ListChat, error) {
	jids, err := d.store.ListMemberJIDs(listID)
	if err != nil {
		return nil, err
	}
	out := make([]ListChat, 0, len(jids))
	for _, raw := range jids {
		item := ListChat{ListJID: raw, JID: raw}
		if jid, err := types.ParseJID(raw); err == nil {
			item.IsGroup = jid.Server == types.GroupServer
			if jid.Server == types.HiddenUserServer && d.pnForLID != nil {
				if pn, err := d.pnForLID(ctx, jid); err == nil && !pn.IsEmpty() {
					item.JID = pn.String()
					item.Phone = pn.User
				}
			}
		}
		item.Name = d.store.chatName(item.JID)
		out = append(out, item)
	}
	return out, nil
}

// editableList returns the list if it exists and is a custom list.
func (d chatListDeps) editableList(id string) (ChatList, error) {
	l, err := d.store.GetChatList(id)
	if err != nil {
		return ChatList{}, err
	}
	if l.ListType != "CUSTOM" {
		return ChatList{}, ErrListNotEditable
	}
	return l, nil
}

// setMembership adds (labeled=true) or removes a chat. Idempotent: it does
// nothing when the chat is already in that state. Returns whether it changed.
func (d chatListDeps) setMembership(ctx context.Context, listID, chat string, labeled bool) (bool, types.JID, error) {
	if _, err := d.editableList(listID); err != nil {
		return false, types.JID{}, err
	}
	target, err := d.listTargetJID(chat)
	if err != nil {
		return false, types.JID{}, err
	}
	in, err := d.store.IsInList(listID, target.String())
	if err != nil {
		return false, target, err
	}
	// Always send: the stored state can be stale (a removal made on the phone
	// only shows up after a full sync), and setting the same value again is
	// harmless. The stored state only decides what "changed" reports.
	if err := d.send(ctx, appstate.BuildLabelChat(target, listID, labeled)); err != nil {
		return false, target, fmt.Errorf("WhatsApp rejected the change: %w", err)
	}
	// The change also comes back through app state; store it now so a read
	// right after this call already sees it.
	err = d.store.StoreLabelChat(&events.LabelAssociationChat{
		JID: target, LabelID: listID, Timestamp: time.Now(),
		Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(labeled)},
	})
	return in != labeled, target, err
}

func listError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrListNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrListNotEditable):
		status = http.StatusForbidden
	case strings.HasPrefix(err.Error(), "WhatsApp rejected"):
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]interface{}{"success": false, "message": err.Error()})
}

// registerChatListEndpoints wires the /api/lists endpoints.
func registerChatListEndpoints(mux *http.ServeMux, auth func(http.HandlerFunc) http.HandlerFunc, d chatListDeps) {
	mux.HandleFunc("GET /api/lists", auth(func(w http.ResponseWriter, r *http.Request) {
		lists, err := d.store.GetChatLists()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "lists": lists})
	}))

	// Re-reads every list and membership from WhatsApp (a full sync of the
	// regular app state patches). Needed once for lists created before the
	// bridge stored them; harmless afterwards.
	mux.HandleFunc("POST /api/lists/sync", auth(func(w http.ResponseWriter, r *http.Request) {
		if err := d.sync(r.Context()); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		lists, err := d.store.GetChatLists()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "lists": lists})
	}))

	mux.HandleFunc("GET /api/lists/{id}/chats", auth(func(w http.ResponseWriter, r *http.Request) {
		list, err := d.store.GetChatList(r.PathValue("id"))
		if err != nil {
			listError(w, err)
			return
		}
		chats, err := d.members(r.Context(), list.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "list": list, "chats": chats})
	}))

	mux.HandleFunc("POST /api/lists/{id}/chats", auth(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Chat string `json:"chat"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "invalid JSON body"})
			return
		}
		changed, target, err := d.setMembership(r.Context(), r.PathValue("id"), req.Chat, true)
		if err != nil {
			listError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "changed": changed, "list_jid": target.String()})
	}))

	mux.HandleFunc("DELETE /api/lists/{id}/chats/{chat}", auth(func(w http.ResponseWriter, r *http.Request) {
		changed, target, err := d.setMembership(r.Context(), r.PathValue("id"), r.PathValue("chat"), false)
		if err != nil {
			listError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "changed": changed, "list_jid": target.String()})
	}))
}

// chatListPatches are the app state patches list mutations may arrive in.
var chatListPatches = []appstate.WAPatchName{appstate.WAPatchRegular, appstate.WAPatchRegularHigh, appstate.WAPatchRegularLow}

// syncChatLists runs a full sync of the regular app state patches, which
// re-dispatches every list and membership mutation to the event handler
// (client.EmitAppStateEventsOnFullSync must be on).
func syncChatLists(ctx context.Context, client *whatsmeow.Client, store *MessageStore) error {
	if client == nil || !client.IsConnected() {
		return fmt.Errorf("not connected to WhatsApp")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	start := time.Now()
	for _, patch := range chatListPatches {
		if err := client.FetchAppState(ctx, patch, true, false); err != nil {
			return fmt.Errorf("app state sync of %s failed: %w", patch, err)
		}
	}
	lists, members, err := store.ReconcileChatLists(start)
	if err != nil {
		return fmt.Errorf("reconciling lists: %w", err)
	}
	if lists+members > 0 {
		fmt.Printf("Chat lists: %d list(s) and %d membership(s) removed on another device\n", lists, members)
	}
	return nil
}

var (
	lastListSyncMu sync.Mutex
	lastListSync   time.Time
)

// syncChatListsOnConnect refreshes the lists after (re)connecting, at most
// every 10 minutes, so removals made on the phone show up without a manual
// sync. Runs in its own goroutine: event handlers must not block.
func syncChatListsOnConnect(client *whatsmeow.Client, store *MessageStore) {
	lastListSyncMu.Lock()
	if time.Since(lastListSync) < 10*time.Minute {
		lastListSyncMu.Unlock()
		return
	}
	lastListSync = time.Now()
	lastListSyncMu.Unlock()
	go func() {
		if err := syncChatLists(context.Background(), client, store); err != nil {
			fmt.Printf("Chat list sync after connect failed: %v\n", err)
		}
	}()
}
