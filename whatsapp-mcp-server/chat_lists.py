"""WhatsApp chat lists ("Favorites", custom lists like "Work").

WhatsApp syncs lists between devices as app state labels; the bridge stores
them in messages.db (tables labels and label_chats), so reads come straight
from SQLite and work while the bridge is down. Adding or removing a chat goes
through the bridge, which sends the change to WhatsApp: it shows up on the
phone and every linked device.
"""

import sqlite3
import unicodedata
from typing import Any
from urllib.parse import quote

import requests

import whatsapp
from phone import digits_only, looks_like_phone


class ChatListError(ValueError):
    """A list or chat could not be resolved safely; nothing was changed."""


class ListLookupError(ChatListError):
    """The list name or id matched no list, or several."""


class ChatLookupError(ChatListError):
    """The chat could not be resolved to exactly one person or group."""


def list_chat_lists() -> list[dict[str, Any]]:
    rows = _query(
        """
        SELECT l.id, l.name, l.list_type, l.order_index,
               (SELECT COUNT(*) FROM label_chats c WHERE c.label_id = l.id AND c.labeled = 1)
          FROM labels l
         WHERE l.deleted = 0
        """
    )
    # WhatsApp's own order first, then by visible name (the raw names of system
    # lists start with an invisible mark that would sort them last).
    rows.sort(key=lambda r: (r[3] is None, r[3] or 0, _clean(r[1]).casefold()))
    return [
        {"id": id_, "name": _clean(name), "type": list_type.lower(), "chat_count": count}
        for id_, name, list_type, _order, count in rows
    ]


def get_chat_list(list_ref: str) -> dict[str, Any]:
    chosen = resolve_list(list_ref)
    rows = _query(
        "SELECT chat_jid FROM label_chats WHERE label_id = ? AND labeled = 1 ORDER BY chat_jid", (chosen["id"],)
    )
    names = dict(_query("SELECT jid, name FROM chats"))
    chats = []
    for (list_jid,) in rows:
        jid, phone = list_jid, None
        if list_jid.endswith("@lid"):
            phone = whatsapp._resolve_lid_to_phone(list_jid)
            if phone:
                jid = f"{phone}@s.whatsapp.net"
        chats.append(
            {
                "jid": jid,
                "list_jid": list_jid,
                "phone": phone,
                "name": _chat_name(jid, names),
                "is_group": list_jid.endswith("@g.us"),
            }
        )
    chats.sort(key=lambda c: ((c["name"] or "").casefold(), c["jid"]))
    return {**chosen, "chats": chats}


def sync_chat_lists() -> dict[str, Any]:
    result = _bridge("POST", "/lists/sync")
    if not result.get("success"):
        return {"success": False, "message": result.get("message", "Bridge error")}
    return {"success": True, "lists": list_chat_lists()}


def add_chat_to_list(list_ref: str, chat: str) -> dict[str, Any]:
    chosen = resolve_list(list_ref)
    result = _bridge("POST", f"/lists/{quote(chosen['id'])}/chats", {"chat": _bridge_chat(chat)})
    return _describe(result, chosen, "added to", "already in")


def remove_chat_from_list(list_ref: str, chat: str) -> dict[str, Any]:
    chosen = resolve_list(list_ref)
    target = quote(_bridge_chat(chat), safe="@")
    result = _bridge("DELETE", f"/lists/{quote(chosen['id'])}/chats/{target}")
    return _describe(result, chosen, "removed from", "not in")


def resolve_list(list_ref: str) -> dict[str, Any]:
    """Find a list by id, or by name ignoring case (then accents too).

    Raises ListLookupError naming the options when nothing or several match.
    """
    ref = list_ref.strip()
    lists = list_chat_lists()
    by_id = [entry for entry in lists if entry["id"] == ref]
    if by_id:
        return by_id[0]
    for key in (str.casefold, _fold):
        matches = [entry for entry in lists if key(entry["name"]) == key(ref)]
        if len(matches) == 1:
            return matches[0]
        if len(matches) > 1:
            ids = ", ".join(f"{m['name']} (id {m['id']})" for m in matches)
            raise ListLookupError(f"{list_ref!r} matches several lists: {ids}. Pass the id instead.")
    names = ", ".join(f"{entry['name']} (id {entry['id']})" for entry in lists) or "none synced yet"
    raise ListLookupError(f"No list named {list_ref!r}. Lists: {names}")


def _bridge_chat(chat: str) -> str:
    """The chat a write targets, without guessing.

    A phone number in any common format becomes the number of its existing
    direct chat (so "11 91234-5678" finds 5511912345678). With no existing chat
    it must carry a country code, or a national number like "31 91234-5678"
    would go out as a Dutch (+31) one. A number matching several contacts is
    refused. JIDs pass through (the bridge checks they are known). The bridge
    turns people into the LID WhatsApp lists use.
    """
    chat = chat.strip()
    if "@" in chat:
        return chat
    if not looks_like_phone(chat):
        raise ChatLookupError(f"{chat!r} is not a phone number or JID")
    matches = whatsapp._contacts_matching_phone(chat)
    phones = sorted({c.phone_number for c in matches if c.phone_number})
    if len(phones) > 1:
        options = ", ".join(f"{c.name or c.phone_number} ({c.phone_number})" for c in matches)
        raise ChatLookupError(f"{chat!r} matches several contacts: {options}. Pass the full number.")
    if matches:
        jid = matches[0].jid
        return jid if jid.endswith("@lid") else jid.split("@")[0]
    digits = digits_only(chat)
    if len(digits) < 12:
        raise ChatLookupError(f"No chat with {chat!r}. Write the number with its country code (e.g. 55 11 91234-5678).")
    return digits


def _describe(result: dict[str, Any], chosen: dict[str, Any], did: str, already: str) -> dict[str, Any]:
    if not result.get("success"):
        return {"success": False, "message": result.get("message", "Bridge error"), "list": chosen["name"]}
    changed = bool(result.get("changed"))
    verb = did if changed else already
    return {
        "success": True,
        "changed": changed,
        "message": f"Chat {verb} list {chosen['name']!r}",
        "list": chosen["name"],
        "list_jid": result.get("list_jid"),
    }


def _bridge(method: str, path: str, payload: dict[str, Any] | None = None) -> dict[str, Any]:
    try:
        response = requests.request(
            method, f"{whatsapp.WHATSAPP_API_BASE_URL}{path}", json=payload, headers=whatsapp._bridge_headers()
        )
    except requests.RequestException as e:
        return {"success": False, "message": f"Bridge unreachable: {e}"}
    try:
        return response.json()
    except ValueError:
        return {"success": False, "message": f"HTTP {response.status_code}: {response.text}"}


def _query(sql: str, params: tuple = ()) -> list[tuple]:
    try:
        conn = sqlite3.connect(whatsapp.MESSAGES_DB_PATH)
        try:
            return conn.execute(sql, params).fetchall()
        finally:
            conn.close()
    except sqlite3.OperationalError as e:
        if "no such table" in str(e):
            return []  # bridge older than chat lists, or not synced yet
        raise


def _chat_name(jid: str, chat_names: dict[str, str]) -> str | None:
    """The contact's name from whatsmeow's contact store, else the chat's
    stored name, unless that is just digits (the bridge stores the number
    when it knows no name)."""
    if not jid.endswith("@g.us"):
        name = whatsapp._resolve_name_from_whatsmeow(jid)
        if name:
            return name
    stored = chat_names.get(jid)
    if stored and not stored.replace("+", "").isdigit():
        return stored
    return None


def _clean(name: str) -> str:
    # WhatsApp prefixes system list names with a left-to-right mark.
    return name.replace("‎", "").strip()


def _fold(text: str) -> str:
    decomposed = unicodedata.normalize("NFD", text.casefold())
    return "".join(ch for ch in decomposed if not unicodedata.combining(ch))
