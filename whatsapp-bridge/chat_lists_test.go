package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func newListStore(t *testing.T) *MessageStore {
	t.Helper()
	ms := newTestMessageStore(t)
	if err := ensureChatListTables(ms.db); err != nil {
		t.Fatal(err)
	}
	return ms
}

func labelEdit(id, name string, typ waSyncAction.LabelEditAction_ListType, deleted bool, at time.Time) *events.LabelEdit {
	return &events.LabelEdit{LabelID: id, Timestamp: at, Action: &waSyncAction.LabelEditAction{
		Name: proto.String(name), Type: typ.Enum(), Deleted: proto.Bool(deleted),
	}}
}

func labelChat(id, jid string, labeled bool, at time.Time) *events.LabelAssociationChat {
	return &events.LabelAssociationChat{LabelID: id, JID: types.NewJID(strings.Split(jid, "@")[0], strings.Split(jid, "@")[1]),
		Timestamp: at, Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(labeled)}}
}

func TestChatListEventsNewestWins(t *testing.T) {
	ms := newListStore(t)
	custom := waSyncAction.LabelEditAction_CUSTOM
	for _, evt := range []interface{}{
		labelEdit("15", "Familia", custom, false, t0),
		labelEdit("15", "Família", custom, false, t0.Add(time.Minute)),
		labelEdit("15", "OLD NAME", custom, false, t0.Add(-time.Hour)), // replayed out of order
		labelEdit("3", "Gone", custom, true, t0),
		labelChat("15", "111@lid", true, t0),
		labelChat("15", "222@lid", true, t0),
		labelChat("15", "222@lid", false, t0.Add(time.Minute)),
		labelChat("15", "222@lid", true, t0.Add(-time.Minute)), // older add must not undo the removal
	} {
		if !handleChatListEvent(ms, evt) {
			t.Fatalf("event %T not handled", evt)
		}
	}
	if handleChatListEvent(ms, &events.Receipt{}) {
		t.Fatal("non-list event reported as handled")
	}
	lists, err := ms.GetChatLists()
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || lists[0].Name != "Família" || lists[0].ListType != "CUSTOM" || lists[0].ChatCount != 1 {
		t.Fatalf("lists = %+v", lists)
	}
	if in, _ := ms.IsInList("15", "222@lid"); in {
		t.Fatal("an older add undid a newer removal")
	}
}

type fakeListWA struct {
	sent    []appstate.PatchInfo
	sendErr error
	resolve map[string]types.JID
	pn      map[types.JID]types.JID
}

func (f *fakeListWA) deps(ms *MessageStore) chatListDeps {
	return chatListDeps{
		store: ms,
		send: func(_ context.Context, p appstate.PatchInfo) error {
			if f.sendErr != nil {
				return f.sendErr
			}
			f.sent = append(f.sent, p)
			return nil
		},
		resolve: func(chat string) (types.JID, error) {
			if jid, ok := f.resolve[chat]; ok {
				return jid, nil
			}
			return types.NewJID(chat, types.DefaultUserServer), nil // no LID known
		},
		pnForLID: func(_ context.Context, lid types.JID) (types.JID, error) { return f.pn[lid], nil },
		sync:     func(context.Context) error { return nil },
	}
}

func listFixture(t *testing.T) (*MessageStore, *fakeListWA, *http.ServeMux) {
	t.Helper()
	ms := newListStore(t)
	for _, evt := range []interface{}{
		labelEdit("15", "Família", waSyncAction.LabelEditAction_CUSTOM, false, t0),
		labelEdit("7", "Favorites", waSyncAction.LabelEditAction_FAVORITES, false, t0),
		labelChat("15", "111@lid", true, t0),
		labelChat("15", "999@g.us", true, t0),
	} {
		handleChatListEvent(ms, evt)
	}
	if err := ms.StoreChat("5531999990000@s.whatsapp.net", "Ana", t0); err != nil {
		t.Fatal(err)
	}
	wa := &fakeListWA{
		resolve: map[string]types.JID{"5531988887777": types.NewJID("222", types.HiddenUserServer)},
		pn:      map[types.JID]types.JID{types.NewJID("111", types.HiddenUserServer): types.NewJID("5531999990000", types.DefaultUserServer)},
	}
	mux := http.NewServeMux()
	registerChatListEndpoints(mux, func(h http.HandlerFunc) http.HandlerFunc { return h }, wa.deps(ms))
	return ms, wa, mux
}

func call(t *testing.T, mux *http.ServeMux, method, path, body string) (int, map[string]interface{}) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: bad JSON: %v", method, path, err)
	}
	return rec.Code, out
}

func TestListChatsEndpointMapsLIDsToPhonesAndNames(t *testing.T) {
	_, _, mux := listFixture(t)
	code, out := call(t, mux, http.MethodGet, "/api/lists/15/chats", "")
	if code != http.StatusOK {
		t.Fatalf("code %d: %v", code, out)
	}
	chats := out["chats"].([]interface{})
	if len(chats) != 2 {
		t.Fatalf("chats = %v", chats)
	}
	person := chats[0].(map[string]interface{})
	if person["list_jid"] != "111@lid" || person["jid"] != "5531999990000@s.whatsapp.net" || person["phone"] != "5531999990000" || person["name"] != "Ana" {
		t.Fatalf("person = %v", person)
	}
	if group := chats[1].(map[string]interface{}); group["is_group"] != true || group["jid"] != "999@g.us" {
		t.Fatalf("group = %v", group)
	}
	if code, _ := call(t, mux, http.MethodGet, "/api/lists/404/chats", ""); code != http.StatusNotFound {
		t.Fatalf("unknown list: code %d", code)
	}
	code, out = call(t, mux, http.MethodGet, "/api/lists", "")
	if code != http.StatusOK || len(out["lists"].([]interface{})) != 2 {
		t.Fatalf("GET /api/lists: %d %v", code, out)
	}
}

func TestAddAndRemoveChatAreIdempotent(t *testing.T) {
	ms, wa, mux := listFixture(t)

	code, out := call(t, mux, http.MethodPost, "/api/lists/15/chats", `{"chat":"5531988887777"}`)
	if code != http.StatusOK || out["changed"] != true || out["list_jid"] != "222@lid" {
		t.Fatalf("add: %d %v", code, out)
	}
	if len(wa.sent) != 1 {
		t.Fatalf("sent %d patches, want 1", len(wa.sent))
	}
	m := wa.sent[0].Mutations[0]
	if wa.sent[0].Type != appstate.WAPatchRegular || strings.Join(m.Index, "|") != "label_jid|15|222@lid" || !m.Value.GetLabelAssociationAction().GetLabeled() {
		t.Fatalf("patch = %+v / %v", wa.sent[0].Type, m.Index)
	}
	if in, _ := ms.IsInList("15", "222@lid"); !in {
		t.Fatal("add not stored locally")
	}

	// Again: reported as no change, but still sent (the stored state may be
	// stale after a removal on the phone; setting it again is harmless).
	if code, out := call(t, mux, http.MethodPost, "/api/lists/15/chats", `{"chat":"5531988887777"}`); code != http.StatusOK || out["changed"] != false {
		t.Fatalf("second add: %d %v", code, out)
	}
	if len(wa.sent) != 2 || !wa.sent[1].Mutations[0].Value.GetLabelAssociationAction().GetLabeled() {
		t.Fatal("second add did not resend the add")
	}

	code, out = call(t, mux, http.MethodDelete, "/api/lists/15/chats/5531988887777", "")
	if code != http.StatusOK || out["changed"] != true || wa.sent[2].Mutations[0].Value.GetLabelAssociationAction().GetLabeled() {
		t.Fatalf("remove: %d %v", code, out)
	}
	if code, out := call(t, mux, http.MethodDelete, "/api/lists/15/chats/5531988887777", ""); code != http.StatusOK || out["changed"] != false {
		t.Fatalf("second remove: %d %v", code, out)
	}
	// Groups and LIDs are accepted as JIDs.
	if code, out := call(t, mux, http.MethodDelete, "/api/lists/15/chats/999@g.us", ""); code != http.StatusOK || out["changed"] != true {
		t.Fatalf("remove group: %d %v", code, out)
	}
	if len(wa.sent) != 5 {
		t.Fatalf("sent %d patches, want 5", len(wa.sent))
	}
}

func TestReconcileRetiresWhatTheSnapshotOmitted(t *testing.T) {
	ms := newListStore(t)
	custom := waSyncAction.LabelEditAction_CUSTOM
	for _, evt := range []interface{}{
		labelEdit("15", "Família", custom, false, t0),
		labelEdit("16", "Removed on phone", custom, false, t0),
		labelChat("15", "111@lid", true, t0),
		labelChat("15", "222@lid", true, t0),
	} {
		handleChatListEvent(ms, evt)
	}
	time.Sleep(5 * time.Millisecond)
	start := time.Now()
	time.Sleep(5 * time.Millisecond)
	// The full sync re-mentions only list 15 and member 111 (older timestamp
	// than stored: values stay, seen_at moves).
	handleChatListEvent(ms, labelEdit("15", "OLD", custom, false, t0.Add(-time.Hour)))
	handleChatListEvent(ms, labelChat("15", "111@lid", true, t0.Add(-time.Hour)))

	nl, nm, err := ms.ReconcileChatLists(start)
	if err != nil || nl != 1 || nm != 1 {
		t.Fatalf("reconcile = %d lists, %d members, %v; want 1, 1", nl, nm, err)
	}
	lists, _ := ms.GetChatLists()
	if len(lists) != 1 || lists[0].ID != "15" || lists[0].Name != "Família" || lists[0].ChatCount != 1 {
		t.Fatalf("lists after reconcile = %+v", lists)
	}
	if in, _ := ms.IsInList("15", "222@lid"); in {
		t.Fatal("member omitted by the snapshot is still in the list")
	}
}

func TestReconcileDoesNothingWhenTheSyncSawNoList(t *testing.T) {
	ms := newListStore(t)
	handleChatListEvent(ms, labelEdit("15", "Família", waSyncAction.LabelEditAction_CUSTOM, false, t0))
	handleChatListEvent(ms, labelChat("15", "111@lid", true, t0))
	time.Sleep(5 * time.Millisecond)
	nl, nm, err := ms.ReconcileChatLists(time.Now())
	if err != nil || nl != 0 || nm != 0 {
		t.Fatalf("empty sync retired %d lists / %d members (%v)", nl, nm, err)
	}
	if lists, _ := ms.GetChatLists(); len(lists) != 1 || lists[0].ChatCount != 1 {
		t.Fatalf("lists = %+v", lists)
	}
}

func TestUnknownJIDsAreRefused(t *testing.T) {
	_, wa, mux := listFixture(t)
	for _, chat := range []string{"123456@g.us", "77777@lid"} {
		if code, out := call(t, mux, http.MethodPost, "/api/lists/15/chats", `{"chat":"`+chat+`"}`); code != http.StatusBadRequest || !strings.Contains(out["message"].(string), "unknown chat") {
			t.Fatalf("%s: %d %v", chat, code, out)
		}
	}
	// A LID with a known phone is accepted even if no chat or list has it yet.
	wa.pn[types.NewJID("888", types.HiddenUserServer)] = types.NewJID("5531977776666", types.DefaultUserServer)
	if code, out := call(t, mux, http.MethodPost, "/api/lists/15/chats", `{"chat":"888@lid"}`); code != http.StatusOK || out["changed"] != true {
		t.Fatalf("known LID: %d %v", code, out)
	}
	if len(wa.sent) != 1 {
		t.Fatalf("sent %d patches, want 1", len(wa.sent))
	}
}

func TestListWritesAreRefusedSafely(t *testing.T) {
	ms, wa, mux := listFixture(t)
	for _, c := range []struct {
		path, body string
		want       int
	}{
		{"/api/lists/7/chats", `{"chat":"5531988887777"}`, http.StatusForbidden},   // system list
		{"/api/lists/404/chats", `{"chat":"5531988887777"}`, http.StatusNotFound},  // unknown list
		{"/api/lists/15/chats", `{"chat":"5531911112222"}`, http.StatusBadRequest}, // no LID known
		{"/api/lists/15/chats", `{"chat":""}`, http.StatusBadRequest},
		{"/api/lists/15/chats", `not json`, http.StatusBadRequest},
	} {
		if code, out := call(t, mux, http.MethodPost, c.path, c.body); code != c.want || out["success"] != false {
			t.Fatalf("POST %s %s: %d %v, want %d", c.path, c.body, code, out, c.want)
		}
	}
	if len(wa.sent) != 0 {
		t.Fatal("a refused request sent a patch")
	}

	wa.sendErr = errors.New("server said no")
	if code, _ := call(t, mux, http.MethodPost, "/api/lists/15/chats", `{"chat":"5531988887777"}`); code != http.StatusBadGateway {
		t.Fatalf("send failure: code %d", code)
	}
	if in, _ := ms.IsInList("15", "222@lid"); in {
		t.Fatal("a failed send was stored as a membership")
	}
}
