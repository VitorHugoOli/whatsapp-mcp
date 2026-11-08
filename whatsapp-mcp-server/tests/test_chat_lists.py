import sqlite3

import pytest

import chat_lists
import whatsapp


class FakeResponse:
    status_code = 200
    text = ""

    def __init__(self, payload):
        self._payload = payload

    def json(self):
        return self._payload


@pytest.fixture
def dbs(tmp_path, monkeypatch):
    messages = tmp_path / "messages.db"
    with sqlite3.connect(messages) as conn:
        conn.executescript(
            """
            CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
            CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
                                   is_from_me BOOLEAN);
            CREATE TABLE labels (id TEXT PRIMARY KEY, name TEXT, color INTEGER, list_type TEXT,
                                 order_index INTEGER, deleted INTEGER, updated_at TIMESTAMP);
            CREATE TABLE label_chats (label_id TEXT, chat_jid TEXT, labeled INTEGER, updated_at TIMESTAMP,
                                      PRIMARY KEY (label_id, chat_jid));
            INSERT INTO labels VALUES ('15', 'Família', 1, 'CUSTOM', NULL, 0, '2026-11-08');
            INSERT INTO labels VALUES ('16', 'Gym', 2, 'CUSTOM', 9, 0, '2026-11-08');
            INSERT INTO labels VALUES ('7', '‎Favorites', 0, 'FAVORITES', NULL, 0, '2026-11-08');
            INSERT INTO labels VALUES ('3', 'Old', 0, 'CUSTOM', NULL, 1, '2026-11-08');
            INSERT INTO labels VALUES ('20', 'Familia', 3, 'CUSTOM', NULL, 0, '2026-11-08');
            INSERT INTO label_chats VALUES ('15', '111@lid', 1, '2026-11-08');
            INSERT INTO label_chats VALUES ('15', '222@lid', 1, '2026-11-08');
            INSERT INTO label_chats VALUES ('15', '999@g.us', 1, '2026-11-08');
            INSERT INTO label_chats VALUES ('15', '333@lid', 0, '2026-11-08');
            INSERT INTO label_chats VALUES ('15', '444@lid', 1, '2026-11-08');
            INSERT INTO chats VALUES ('5531977776666@s.whatsapp.net', '444', '2026-11-01 10:00:00+00:00');
            INSERT INTO chats VALUES ('5531999990000@s.whatsapp.net', 'Ana', '2026-11-01 10:00:00+00:00');
            INSERT INTO chats VALUES ('999@g.us', 'Book Club', '2026-11-01 10:00:00+00:00');
            """
        )
    whatsmeow = tmp_path / "whatsapp.db"
    with sqlite3.connect(whatsmeow) as conn:
        conn.executescript(
            """
            CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT);
            INSERT INTO whatsmeow_lid_map VALUES ('111', '5531999990000');
            INSERT INTO whatsmeow_lid_map VALUES ('444', '5531977776666');
            CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT,
                                             push_name TEXT, business_name TEXT);
            INSERT INTO whatsmeow_contacts VALUES ('me', '5531977776666@s.whatsapp.net', 'Carla', 'Carla Souza',
                                                   'Carlinha', NULL);
            """
        )
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(messages))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(whatsmeow))
    return messages


def test_lists_hide_deleted_and_clean_system_names(dbs):
    lists = chat_lists.list_chat_lists()
    assert [(entry["name"], entry["type"], entry["chat_count"]) for entry in lists] == [
        ("Gym", "custom", 0),
        ("Familia", "custom", 0),
        ("Família", "custom", 4),
        ("Favorites", "favorites", 0),
    ]


def test_resolve_list_by_id_name_case_and_accents(dbs, monkeypatch):
    assert chat_lists.resolve_list("15")["name"] == "Família"
    assert chat_lists.resolve_list("família")["id"] == "15"  # exact, ignoring case, wins over the accent-free "Familia"
    assert chat_lists.resolve_list("FAMILIA")["id"] == "20"
    assert chat_lists.resolve_list("gym")["id"] == "16"
    with pytest.raises(chat_lists.ListLookupError, match="No list named 'Nope'.*Família \\(id 15\\)"):
        chat_lists.resolve_list("Nope")


def test_ambiguous_name_lists_the_options(dbs):
    with sqlite3.connect(dbs) as conn:
        conn.execute("INSERT INTO labels VALUES ('21', 'FAMÍLIA', 4, 'CUSTOM', NULL, 0, '2026-11-08')")
    with pytest.raises(chat_lists.ListLookupError, match="several lists") as err:
        chat_lists.resolve_list("família")
    assert "id 15" in str(err.value) and "id 21" in str(err.value)


def test_get_chat_list_maps_lids_to_phones_and_names(dbs):
    result = chat_lists.get_chat_list("Família")
    assert result["id"] == "15"
    assert [(c["jid"], c["phone"], c["name"], c["is_group"]) for c in result["chats"]] == [
        ("222@lid", None, None, False),  # no phone known: stays as its LID
        ("5531999990000@s.whatsapp.net", "5531999990000", "Ana", False),
        ("999@g.us", None, "Book Club", True),
        # The chat's stored name is just digits: the contact store's name wins.
        ("5531977776666@s.whatsapp.net", "5531977776666", "Carla Souza", False),
    ]
    assert result["chats"][1]["list_jid"] == "111@lid"


def test_add_resolves_phone_and_calls_bridge(dbs, monkeypatch):
    calls = []
    monkeypatch.setattr(
        whatsapp,
        "_contacts_matching_phone",
        lambda phone: [whatsapp.Contact("5531988887777", "Bia", "5531988887777@s.whatsapp.net")],
    )

    def fake_request(method, url, json=None, headers=None):
        calls.append((method, url, json))
        return FakeResponse({"success": True, "changed": True, "list_jid": "444@lid"})

    monkeypatch.setattr(chat_lists.requests, "request", fake_request)
    result = chat_lists.add_chat_to_list("familia", "(31) 98888-7777")
    assert result == {
        "success": True,
        "changed": True,
        "message": "Chat added to list 'Familia'",
        "list": "Familia",
        "list_jid": "444@lid",
    }
    assert calls == [("POST", f"{whatsapp.WHATSAPP_API_BASE_URL}/lists/20/chats", {"chat": "5531988887777"})]


def test_remove_quotes_the_jid_and_reports_no_change(dbs, monkeypatch):
    calls = []

    def fake_request(method, url, json=None, headers=None):
        calls.append((method, url))
        return FakeResponse({"success": True, "changed": False, "list_jid": "999@g.us"})

    monkeypatch.setattr(chat_lists.requests, "request", fake_request)
    result = chat_lists.remove_chat_from_list("15", "999@g.us")
    assert result["success"] and not result["changed"]
    assert result["message"] == "Chat not in list 'Família'"
    assert calls == [("DELETE", f"{whatsapp.WHATSAPP_API_BASE_URL}/lists/15/chats/999@g.us")]


def test_bridge_errors_and_unreachable_bridge(dbs, monkeypatch):
    monkeypatch.setattr(
        chat_lists.requests,
        "request",
        lambda *a, **k: FakeResponse({"success": False, "message": "only custom lists can be edited"}),
    )
    result = chat_lists.add_chat_to_list("Favorites", "999@g.us")
    assert result == {"success": False, "message": "only custom lists can be edited", "list": "Favorites"}

    def boom(*a, **k):
        raise chat_lists.requests.ConnectionError("refused")

    monkeypatch.setattr(chat_lists.requests, "request", boom)
    assert "Bridge unreachable" in chat_lists.add_chat_to_list("15", "999@g.us")["message"]


def test_missing_tables_mean_no_lists(tmp_path, monkeypatch):
    path = tmp_path / "old.db"
    sqlite3.connect(path).close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    assert chat_lists.list_chat_lists() == []
    with pytest.raises(chat_lists.ListLookupError, match="none synced yet"):
        chat_lists.resolve_list("Família")


def _no_bridge(monkeypatch):
    def fail(*a, **k):
        raise AssertionError("bridge must not be called")

    monkeypatch.setattr(chat_lists.requests, "request", fail)


def test_ambiguous_phone_is_refused(dbs, monkeypatch):
    _no_bridge(monkeypatch)
    monkeypatch.setattr(
        whatsapp,
        "_contacts_matching_phone",
        lambda phone: [
            whatsapp.Contact("5531988887777", "Bia", "5531988887777@s.whatsapp.net"),
            whatsapp.Contact("5511988887777", "Outra Bia", "5511988887777@s.whatsapp.net"),
        ],
    )
    with pytest.raises(chat_lists.ChatLookupError, match="several contacts"):
        chat_lists.add_chat_to_list("15", "98888-7777")


def test_national_number_without_a_chat_is_refused(dbs, monkeypatch):
    _no_bridge(monkeypatch)
    monkeypatch.setattr(whatsapp, "_contacts_matching_phone", lambda phone: [])
    with pytest.raises(chat_lists.ChatLookupError, match="country code"):
        chat_lists.add_chat_to_list("15", "31 91234-5678")  # would be +31 (Netherlands)
    with pytest.raises(chat_lists.ChatLookupError, match="not a phone number"):
        chat_lists.add_chat_to_list("15", "Ana")


def test_full_number_without_a_chat_and_jids_pass_through(dbs, monkeypatch):
    calls = []
    monkeypatch.setattr(whatsapp, "_contacts_matching_phone", lambda phone: [])

    def fake_request(method, url, json=None, headers=None):
        calls.append(json)
        return FakeResponse({"success": True, "changed": True, "list_jid": "1@lid"})

    monkeypatch.setattr(chat_lists.requests, "request", fake_request)
    chat_lists.add_chat_to_list("15", "+55 11 91234-5678")
    chat_lists.add_chat_to_list("15", "999@g.us")
    assert calls == [{"chat": "5511912345678"}, {"chat": "999@g.us"}]


def test_sync_calls_the_bridge_and_returns_lists(dbs, monkeypatch):
    calls = []

    def fake_request(method, url, json=None, headers=None):
        calls.append((method, url))
        return FakeResponse({"success": True})

    monkeypatch.setattr(chat_lists.requests, "request", fake_request)
    result = chat_lists.sync_chat_lists()
    assert result["success"] and len(result["lists"]) == 4
    assert calls == [("POST", f"{whatsapp.WHATSAPP_API_BASE_URL}/lists/sync")]
