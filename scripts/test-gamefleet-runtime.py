#!/usr/bin/env python3
"""Isolated live Nakama/GameFleet plugin load check. Uses temporary containers and an ephemeral PostgreSQL database."""
from __future__ import annotations

import base64
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import select
import secrets
import shutil
import socket
import subprocess
import struct
import tempfile
import time
import urllib.error
import urllib.request
from urllib.parse import quote, urlsplit
import uuid


REPO = Path.cwd()
RUNTIME_IMAGE = os.environ.get("GF_P4D_RUNTIME_IMAGE", "nakama-gamefleet-p4d:924ddf85")
POSTGRES_IMAGE = "postgres:16-alpine"
SYNTHETIC_KEY = "gfbiz_p4d_synthetic_only_0123456789abcdef"
BACKEND = "gamefleet"
SERVICE_ID = "service_p4d_fixture"
IDENTITY_ISSUER = "issuer_p4d_fixture"
APP_ID = "app_p4d_fixture"
PLACEMENT_ID = "placement_p4d_fixture"
REVISION_ID = "revision_p4d_fixture"
REGION = "p4d-local"
COMPATIBILITY = "p4d-smoke"
ROOM_VERSION = "gamefleet.player-room.v1"
SEARCH_VERSION = "gamefleet.player-search.v1"
EVIDENCE_PATH = Path(tempfile.gettempdir()) / "gamefleet-runtime-smoke-evidence.json"
db_password = ""

GO_FIXTURE = r'''package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	appID = "app_p4d_fixture"
	placementID = "placement_p4d_fixture"
	revisionID = "revision_p4d_fixture"
	region = "p4d-local"
	roomVersion = "gamefleet.player-room.v1"
	searchVersion = "gamefleet.player-search.v1"
	serviceSearchVersion = "gamefleet.service-player-search.v1"
)

type fixtureSearch struct {
	ID string
	ParticipantID string
	State string
	CreatedAt time.Time
	ExpiresAt time.Time
	ResolvedAt *time.Time
	AllocationID string
}

type fixtureReservation struct {
	ReservationID string `json:"reservationId"`
	AllocationID string `json:"allocationId"`
	RoomID string `json:"roomId"`
	ApplicationID string `json:"applicationId"`
	PlacementID string `json:"placementId"`
	RevisionID string `json:"revisionId"`
	Region string `json:"region"`
	State string `json:"state"`
	CancellationRequested bool `json:"cancellationRequested"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func decodeFixtureJSON(r *http.Request, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return fmt.Errorf("invalid request size")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(out); err != nil || d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("invalid request body")
	}
	return nil
}

func searchObject(s *fixtureSearch, reservation *fixtureReservation) map[string]any {
	out := map[string]any{
		"searchId": s.ID, "state": s.State, "region": region, "compatibility": "p4d-smoke",
		"createdAt": s.CreatedAt, "expiresAt": s.ExpiresAt,
	}
	if s.ResolvedAt != nil {
		out["resolvedAt"] = s.ResolvedAt
	}
	if s.AllocationID != "" {
		out["allocationId"] = s.AllocationID
	}
	if reservation != nil {
		out["reservation"] = reservation
	}
	return out
}

func writeBusiness(w http.ResponseWriter, status int, data any, requestID string) {
	writeJSON(w, status, map[string]any{"data": data, "requestId": requestID})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func main() {
	key := os.Getenv("GF_P4D_FIXTURE_KEY")
	mode := os.Getenv("GF_P4D_FIXTURE_MODE")
	service := os.Getenv("GF_P4D_FIXTURE_BACKEND") == "gamefleet-service"
	scopePath, currentPath, beginPath := "/business/v1/caller", "/business/v1/reservations/current", "/business/v1/searches"
	beginVersion := searchVersion
	if service {
		scopePath, currentPath, beginPath = "/business/v1/history/service", "/business/v1/history/reservations/current", "/business/v1/service-searches"
		beginVersion = serviceSearchVersion
	}
	var mu sync.Mutex
	searches := map[string]*fixtureSearch{}
	requestIDs := map[string]string{}
	matchHashes := map[string]string{}
	matchReservations := map[string]*fixtureReservation{}
	participantRooms := map[string]*fixtureReservation{}
	searchSequence := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc(scopePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Print("p4d_fixture_bad_caller_auth")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		if mode == "deny" {
			log.Print("p4d_fixture_scope_denied")
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "fixture_denied"})
			return
		}
		log.Print("p4d_fixture_scope_ok")
		if service {
			operations := []string{"read", "cancel", "assignment", "resume"}
			if mode == "history-read-only" {
				operations = []string{"read"}
				log.Print("p4d_fixture_scope_read_only")
			}
			writeBusiness(w, http.StatusOK, map[string]any{
				"serviceId": "service_p4d_fixture", "applicationId": appID,
				"identityIssuer": "issuer_p4d_fixture", "operations": operations,
			}, "p4d-fixture-service")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"callerId": "caller_p4d_fixture", "applicationId": appID,
				"placementId": placementID, "revisionId": revisionID,
				"region": region, "operations": []string{"reserve", "read", "cancel", "assignment", "resume"},
			},
			"requestId": "p4d-fixture-caller",
		})
	})
	mux.HandleFunc(currentPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Print("p4d_fixture_bad_current_auth")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		var request struct {
			Version string `json:"version"`
			ParticipantID string `json:"participantId"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(body) > 4096 || json.Unmarshal(body, &request) != nil || request.Version != roomVersion || request.ParticipantID == "" {
			log.Print("p4d_fixture_bad_current_request")
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_fixture_request"})
			return
		}
		mu.Lock()
		reservation := participantRooms[request.ParticipantID]
		mu.Unlock()
		if reservation == nil {
			log.Printf("p4d_fixture_current_null user=%s", request.ParticipantID)
			writeBusiness(w, http.StatusOK, map[string]any{"current": nil}, "p4d-fixture-current")
			return
		}
		log.Printf("p4d_fixture_current_bound user=%s allocation=%s", request.ParticipantID, reservation.AllocationID)
		writeBusiness(w, http.StatusOK, map[string]any{"current": map[string]any{"reservation": reservation, "connectionGeneration": 0}}, "p4d-fixture-current")
	})
	mux.HandleFunc(beginPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Print("p4d_fixture_bad_search_auth")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		var request struct {
			Version string `json:"version"`
			ParticipantID string `json:"participantId"`
			RequestID string `json:"requestId"`
			Compatibility string `json:"compatibility"`
			Region string `json:"region"`
		}
		if decodeFixtureJSON(r, &request) != nil || request.Version != beginVersion || request.Compatibility != "p4d-smoke" || request.RequestID == "" || request.ParticipantID == "" || (service && request.Region != region) || (!service && request.Region != "") {
			log.Print("p4d_fixture_bad_search_begin")
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_fixture_request"})
			return
		}
		mu.Lock()
		requestKey := request.ParticipantID + "\x00" + request.RequestID
		id := requestIDs[requestKey]
		replay := id != ""
		if !replay {
			searchSequence++
			id = fmt.Sprintf("search_p4d_%d", searchSequence)
			created := time.Now().UTC().Truncate(time.Millisecond)
			searches[id] = &fixtureSearch{ID: id, ParticipantID: request.ParticipantID, State: "pending", CreatedAt: created, ExpiresAt: created.Add(120 * time.Second)}
			requestIDs[requestKey] = id
		}
		search := searches[id]
		result := map[string]any{"search": searchObject(search, nil), "replay": replay}
		mu.Unlock()
		log.Printf("p4d_fixture_search_begin user=%s state=%s replay=%t", request.ParticipantID, search.State, replay)
		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		writeBusiness(w, status, result, "p4d-fixture-search-begin")
	})
	mux.HandleFunc(beginPath+"/match", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Print("p4d_fixture_bad_match_auth")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		var request struct {
			Version string `json:"version"`
			IdempotencyKey string `json:"idempotencyKey"`
			Compatibility string `json:"compatibility"`
			Region string `json:"region"`
			Members []struct {
				ParticipantID string `json:"participantId"`
				SearchID string `json:"searchId"`
				NakamaTicket string `json:"nakamaTicket"`
			} `json:"members"`
		}
		if decodeFixtureJSON(r, &request) != nil || request.Version != beginVersion || request.Compatibility != "p4d-smoke" || request.IdempotencyKey == "" || len(request.Members) != 2 || (service && request.Region != region) || (!service && request.Region != "") {
			log.Print("p4d_fixture_bad_search_match")
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_fixture_request"})
			return
		}
		hashInput, _ := json.Marshal(request.Members)
		hash := string(hashInput)
		mu.Lock()
		if oldHash, ok := matchHashes[request.IdempotencyKey]; ok {
			if oldHash != hash {
				mu.Unlock()
				writeJSON(w, http.StatusConflict, map[string]any{"error": "idempotency_conflict"})
				return
			}
			reservation := matchReservations[request.IdempotencyKey]
			mu.Unlock()
			log.Print("p4d_fixture_search_match replay=true")
			writeBusiness(w, http.StatusOK, map[string]any{"reservation": reservation, "replay": true}, "p4d-fixture-search-match")
			return
		}
		first, second := searches[request.Members[0].SearchID], searches[request.Members[1].SearchID]
		if first == nil || second == nil || first.ParticipantID != request.Members[0].ParticipantID || second.ParticipantID != request.Members[1].ParticipantID || first.ID == second.ID || request.Members[0].NakamaTicket == "" || request.Members[1].NakamaTicket == "" || request.Members[0].NakamaTicket == request.Members[1].NakamaTicket || first.State != "pending" || second.State != "pending" {
			mu.Unlock()
			log.Print("p4d_fixture_search_match rejected=true")
			writeJSON(w, http.StatusConflict, map[string]any{"error": "search_unavailable"})
			return
		}
		matchedAt := time.Now().UTC().Truncate(time.Millisecond)
		reservation := &fixtureReservation{
			ReservationID: "reservation_p4d_smoke", AllocationID: "allocation_p4d_smoke", RoomID: "room_p4d_smoke",
			ApplicationID: appID, PlacementID: placementID, RevisionID: revisionID, Region: region,
			State: "reserved", CreatedAt: matchedAt, UpdatedAt: matchedAt,
		}
		for _, search := range []*fixtureSearch{first, second} {
			search.State = "bound"
			search.ResolvedAt = &matchedAt
			search.AllocationID = reservation.AllocationID
			participantRooms[search.ParticipantID] = reservation
		}
		matchHashes[request.IdempotencyKey] = hash
		matchReservations[request.IdempotencyKey] = reservation
		mu.Unlock()
		log.Printf("p4d_fixture_search_match replay=false allocation=%s", reservation.AllocationID)
		writeBusiness(w, http.StatusAccepted, map[string]any{"reservation": reservation, "replay": false}, "p4d-fixture-search-match")
	})
	searchHandler := func(prefix, expectedVersion, use string) http.HandlerFunc { return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method"})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			log.Print("p4d_fixture_bad_search_auth")
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		parts := strings.Split(path, "/")
		if len(parts) != 2 || (parts[1] != "status" && parts[1] != "cancel") || (use == "mapped" && parts[1] == "cancel") {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
			return
		}
		var request struct {
			Version string `json:"version"`
			ParticipantID string `json:"participantId"`
		}
		if decodeFixtureJSON(r, &request) != nil || request.Version != expectedVersion || request.ParticipantID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_fixture_request"})
			return
		}
		mu.Lock()
		search := searches[parts[0]]
		if search == nil || search.ParticipantID != request.ParticipantID {
			mu.Unlock()
			log.Printf("p4d_fixture_search_owner_denied participant=%s", request.ParticipantID)
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "search_not_found"})
			return
		}
		if parts[1] == "cancel" {
			replay := search.State != "pending"
			if !replay {
				now := time.Now().UTC().Truncate(time.Millisecond)
				search.State = "cancelled"
				search.ResolvedAt = &now
			}
			result := map[string]any{"search": searchObject(search, participantRooms[request.ParticipantID]), "replay": replay}
			mu.Unlock()
			log.Printf("p4d_fixture_search_cancel user=%s state=%s replay=%t", request.ParticipantID, search.State, replay)
			log.Printf("p4d_fixture_search_route use=%s operation=cancel", use)
			writeBusiness(w, http.StatusOK, result, "p4d-fixture-search-cancel")
			return
		}
		state := search.State
		statusResult := map[string]any{"search": searchObject(search, participantRooms[request.ParticipantID])}
		mu.Unlock()
		log.Printf("p4d_fixture_search_status user=%s state=%s", request.ParticipantID, state)
		log.Printf("p4d_fixture_search_route use=%s operation=status", use)
		writeBusiness(w, http.StatusOK, statusResult, "p4d-fixture-search-status")
	} }
	if service {
		mux.HandleFunc(beginPath+"/", searchHandler(beginPath+"/", serviceSearchVersion, "mapped"))
		mux.HandleFunc("/business/v1/history/searches/", searchHandler("/business/v1/history/searches/", searchVersion, "history"))
	} else {
		mux.HandleFunc(beginPath+"/", searchHandler(beginPath+"/", searchVersion, "ordinary"))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("p4d_fixture_unexpected method=%s path=%s", r.Method, r.URL.Path)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not_found"})
	})
	listener, err := net.Listen("tcp", "127.0.0.1:17682")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("p4d_fixture_ready")
	log.Fatal(http.Serve(listener, mux))
}
'''


class SmokeError(RuntimeError):
    pass


def docker(args: list[str], *, check: bool = True, timeout: int = 120) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(["docker", *args], text=True, capture_output=True, timeout=timeout)
    if check and result.returncode:
        stderr, stdout = result.stderr, result.stdout
        for secret in (db_password, SYNTHETIC_KEY):
            if secret:
                replacement = "<redacted-db-password>" if secret == db_password else "<synthetic-key>"
                stderr = stderr.replace(secret, replacement)
                stdout = stdout.replace(secret, replacement)
        raise SmokeError(f"docker {args[0]} failed ({result.returncode}): {stderr[-4000:]} {stdout[-1000:]}")
    return result


def image_facts(image: str) -> dict[str, str]:
    result = docker(["image", "inspect", image])
    inspected = json.loads(result.stdout)[0]
    config = inspected.get("Config") or {}
    return {
        "id": inspected["Id"],
        "platform": inspected["Os"] + "/" + inspected["Architecture"],
        "labels": config.get("Labels") or {},
        "repo_digests": inspected.get("RepoDigests") or [],
    }


def container_state(name: str) -> tuple[str, int]:
    result = docker(["inspect", "--format", "{{.State.Status}}|{{.State.ExitCode}}", name], check=False)
    if result.returncode:
        return "missing", -1
    status, code = result.stdout.strip().split("|", 1)
    return status, int(code)


def logs(name: str) -> str:
    result = docker(["logs", name], check=False, timeout=15)
    return result.stdout + result.stderr


def wait_for_db(name: str, deadline: float) -> None:
    while time.monotonic() < deadline:
        result = docker(["exec", name, "pg_isready", "-U", "postgres", "-d", "nakama"], check=False, timeout=10)
        if result.returncode == 0:
            return
        status, _ = container_state(name)
        if status != "running":
            raise SmokeError("temporary PostgreSQL exited before becoming ready")
        time.sleep(1)
    raise SmokeError("temporary PostgreSQL readiness timed out")


def wait_for_fixture(name: str, deadline: float) -> None:
    while time.monotonic() < deadline:
        if "p4d_fixture_ready" in logs(name):
            return
        status, _ = container_state(name)
        if status != "running":
            raise SmokeError("loopback Business fixture exited before listening")
        time.sleep(0.25)
    raise SmokeError("loopback Business fixture readiness timed out")


def get_url(url: str, timeout: float = 2.0) -> tuple[int, bytes]:
    req = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            return response.status, response.read(16384)
    except urllib.error.HTTPError as error:
        return error.code, error.read(16384)
    except (urllib.error.URLError, http.client.HTTPException, OSError, TimeoutError, socket.timeout):
        return 0, b""


def safe_log_excerpt(raw: str) -> list[str]:
    for secret, replacement in ((db_password, "<redacted-db-password>"), (SYNTHETIC_KEY, "<synthetic-key>")):
        if secret:
            raw = raw.replace(secret, replacement)
    terms = ("gamefleet", "plugin", "runtime module", "nakama", "error", "fatal")
    return [line[:1000] for line in raw.splitlines() if any(term in line.lower() for term in terms)][-30:]


def wait_health(url: str, name: str, deadline: float) -> bool:
    while time.monotonic() < deadline:
        status, _ = get_url(url)
        if status == 200:
            return True
        state, _ = container_state(name)
        if state != "running":
            return False
        time.sleep(0.5)
    return get_url(url)[0] == 200


def post_json(url: str, body: bytes, headers: dict[str, str], timeout: float = 10.0) -> tuple[int, dict[str, object]]:
    request = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json", **headers}, method="POST")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read(65536)
            return response.status, json.loads(raw)
    except urllib.error.HTTPError as error:
        try:
            payload = json.loads(error.read(65536))
        except Exception:
            payload = {}
        return error.code, payload


class NakamaWebSocket:
    """Small stdlib JSON WebSocket client for the isolated matchmaker smoke."""

    def __init__(self, base_url: str, token: str) -> None:
        parsed = urlsplit(base_url)
        if parsed.scheme != "http" or parsed.hostname is None or parsed.port is None:
            raise SmokeError("smoke WebSocket requires the local HTTP API origin")
        self.sock = socket.create_connection((parsed.hostname, parsed.port), timeout=10)
        self.sock.settimeout(10)
        self.buffer = bytearray()
        key = base64.b64encode(secrets.token_bytes(16)).decode("ascii")
        path = "/ws?lang=en&status=true&format=json&token=" + quote(token, safe="")
        request = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {parsed.hostname}:{parsed.port}\r\n"
            "Upgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
            "Origin: http://127.0.0.1\r\n\r\n"
        ).encode("ascii")
        self.sock.sendall(request)
        response = bytearray()
        while b"\r\n\r\n" not in response and len(response) < 16384:
            part = self.sock.recv(4096)
            if not part:
                self.sock.close()
                raise SmokeError("Nakama WebSocket closed during handshake")
            response.extend(part)
        header, separator, extra = response.partition(b"\r\n\r\n")
        if not separator or not header.startswith(b"HTTP/1.1 101 "):
            self.sock.close()
            raise SmokeError("Nakama WebSocket upgrade was rejected")
        headers = {}
        for line in header.decode("latin1").split("\r\n")[1:]:
            if ":" in line:
                name, value = line.split(":", 1)
                headers[name.strip().lower()] = value.strip()
        expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode("ascii")).digest()).decode("ascii")
        if headers.get("sec-websocket-accept") != expected:
            self.sock.close()
            raise SmokeError("Nakama WebSocket handshake accept did not match")
        self.buffer.extend(extra)

    def _read_exact(self, length: int) -> bytes:
        while len(self.buffer) < length:
            part = self.sock.recv(max(4096, length - len(self.buffer)))
            if not part:
                raise SmokeError("Nakama WebSocket closed unexpectedly")
            self.buffer.extend(part)
        out = bytes(self.buffer[:length])
        del self.buffer[:length]
        return out

    def _send_frame(self, opcode: int, payload: bytes) -> None:
        mask = secrets.token_bytes(4)
        length = len(payload)
        if length < 126:
            header = bytes((0x80 | opcode, 0x80 | length))
        elif length <= 0xFFFF:
            header = bytes((0x80 | opcode, 0x80 | 126)) + struct.pack("!H", length)
        else:
            header = bytes((0x80 | opcode, 0x80 | 127)) + struct.pack("!Q", length)
        masked = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
        self.sock.sendall(header + mask + masked)

    def send_json(self, value: dict[str, object]) -> None:
        self._send_frame(1, json.dumps(value, separators=(",", ":")).encode("utf-8"))

    def recv_json(self, timeout: float = 1.0) -> dict[str, object] | None:
        deadline = time.monotonic() + timeout
        fragments = bytearray()
        message_opcode = 0
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return None
            if len(self.buffer) < 2 and not select.select([self.sock], [], [], remaining)[0]:
                return None
            first, second = self._read_exact(2)
            opcode = first & 0x0F
            final = bool(first & 0x80)
            masked = bool(second & 0x80)
            length = second & 0x7F
            if length == 126:
                length = struct.unpack("!H", self._read_exact(2))[0]
            elif length == 127:
                length = struct.unpack("!Q", self._read_exact(8))[0]
            mask = self._read_exact(4) if masked else b""
            payload = self._read_exact(length)
            if masked:
                payload = bytes(value ^ mask[index % 4] for index, value in enumerate(payload))
            if opcode == 8:
                return None
            if opcode == 9:
                self._send_frame(10, payload)
                continue
            if opcode == 10:
                continue
            if opcode in (1, 2):
                message_opcode = opcode
                fragments = bytearray(payload)
            elif opcode == 0 and message_opcode:
                fragments.extend(payload)
            else:
                continue
            if final:
                if message_opcode != 1:
                    return None
                value = json.loads(fragments.decode("utf-8"))
                return value if isinstance(value, dict) else None

    def close(self) -> None:
        try:
            self._send_frame(8, b"")
        except OSError:
            pass
        self.sock.close()


def rpc_call(base_url: str, token: str, name: str, payload: dict[str, object]) -> tuple[int, dict[str, object]]:
    # Nakama's HTTP RPC API takes the plugin payload as a JSON string.
    wire = json.dumps(json.dumps(payload, separators=(",", ":")), separators=(",", ":")).encode("utf-8")
    return post_json(
        base_url + "/v2/rpc/" + name,
        wire,
        {"Authorization": "Bearer " + token},
    )


def rpc_payload(base_url: str, token: str, name: str, payload: dict[str, object]) -> dict[str, object]:
    status, response = rpc_call(base_url, token, name, payload)
    if status != 200 or not isinstance(response.get("payload"), str):
        raise SmokeError(f"GameFleet {name} RPC failed with HTTP {status}")
    try:
        result = json.loads(response["payload"])
    except Exception as exc:
        raise SmokeError(f"GameFleet {name} RPC returned a non-JSON payload") from exc
    if not isinstance(result, dict):
        raise SmokeError(f"GameFleet {name} RPC returned a non-object payload")
    return result


def search_begin_rpc(base_url: str, token: str, request_id: str) -> dict[str, object]:
    result = rpc_payload(base_url, token, "gamefleet_search_begin_v1", {
        "version": SEARCH_VERSION, "compatibility": COMPATIBILITY, "region": REGION, "requestId": request_id,
    })
    search = result.get("search")
    if not isinstance(search, dict) or search.get("state") != "pending" or not isinstance(search.get("searchId"), str):
        raise SmokeError("search begin did not return a pending exact search")
    return search


def search_participant_rpc(base_url: str, token: str, rpc_name: str, search_id: str) -> dict[str, object]:
    return rpc_payload(base_url, token, rpc_name, {
        "version": SEARCH_VERSION, "compatibility": COMPATIBILITY, "region": REGION, "searchId": search_id,
    })


def expect_rpc_denied(base_url: str, token: str, name: str, payload: dict[str, object]) -> None:
    status, response = rpc_call(base_url, token, name, payload)
    if status != 200:
        return
    if "error" in response:
        return
    raw = response.get("payload")
    if not isinstance(raw, str):
        return
    try:
        result = json.loads(raw)
    except Exception:
        return
    if not isinstance(result, dict) or not isinstance(result.get("search"), dict):
        return
    raise SmokeError(f"GameFleet {name} unexpectedly authorized the request")


def wait_matchmaker_result(sockets: list[NakamaWebSocket], *, matched: bool, timeout: float = 30.0) -> None:
    pending = set(sockets)
    deadline = time.monotonic() + timeout
    while pending and time.monotonic() < deadline:
        ready, _, _ = select.select([ws.sock for ws in pending], [], [], min(0.5, deadline - time.monotonic()))
        for sock in ready:
            ws = next(candidate for candidate in pending if candidate.sock is sock)
            message = ws.recv_json(0.1)
            if message is None:
                continue
            if "error" in message:
                if matched:
                    raise SmokeError("Nakama rejected a pending owned search in MatchmakerAdd")
                pending.remove(ws)
                continue
            if "matchmaker_matched" in message:
                if not matched:
                    raise SmokeError("cancelled search unexpectedly reached matched callback")
                pending.remove(ws)
    if pending:
        raise SmokeError("timed out waiting for Nakama matchmaker result")


def authenticate(base_url: str, device_id: str) -> str:
    basic = base64.b64encode(b"local-server-key:").decode("ascii")
    status, response = post_json(
        base_url + "/v2/account/authenticate/device?create=true",
        json.dumps({"id": device_id}).encode("utf-8"),
        {"Authorization": "Basic " + basic},
    )
    token = response.get("token")
    if status != 200 or not isinstance(token, str) or not token:
        raise SmokeError(f"synthetic device authentication failed with HTTP {status}")
    return token


def current_rpc(base_url: str, token: str, *, expect_success: bool, expected_allocation: str | None = None) -> tuple[int, dict[str, object]]:
    payload = {"version": ROOM_VERSION, "compatibility": COMPATIBILITY, "region": REGION}
    status, response = rpc_call(base_url, token, "gamefleet_current_v1", payload)
    if expect_success:
        if status != 200 or not isinstance(response.get("payload"), str):
            raise SmokeError(f"GameFleet current RPC failed with HTTP {status}")
        try:
            current = json.loads(response["payload"])
        except Exception as exc:
            raise SmokeError("GameFleet current RPC returned a non-JSON payload") from exc
        if expected_allocation is None:
            if current != {"current": None}:
                raise SmokeError("current RPC did not return the fixture's expected empty state")
        else:
            state = current.get("current") if isinstance(current, dict) else None
            reservation = state.get("reservation") if isinstance(state, dict) else None
            if not isinstance(reservation, dict) or reservation.get("allocationId") != expected_allocation or reservation.get("cancellationRequested") is not False:
                raise SmokeError("current RPC lost or cancelled the search-bound room")
    return status, response


def start_fixture(name: str, image: str, mode: str) -> None:
    docker([
        "run", "--detach", "--pull=never", "--platform", "linux/amd64", "--name", name,
        "--network", "container:" + db_container,
        "--env", "GF_P4D_FIXTURE_KEY=" + SYNTHETIC_KEY,
        "--env", "GF_P4D_FIXTURE_MODE=" + mode,
        "--env", "GF_P4D_FIXTURE_BACKEND=" + BACKEND,
        image,
    ])
    wait_for_fixture(name, time.monotonic() + 15)


def start_nakama(name: str, config_path: Path, key_path: Path) -> None:
    dsn = f"postgres:{db_password}@127.0.0.1:5432/nakama?sslmode=disable"
    env = {
        "NAKAMA_FLEET_BACKEND": BACKEND,
        "GAMEFLEET_BUSINESS_URL": "http://127.0.0.1:17682",
        "GAMEFLEET_BUSINESS_KEY_FILE": "/run/secrets/gamefleet-business-key",
        "GAMEFLEET_APPLICATION_ID": APP_ID,
        "GAMEFLEET_PLACEMENT_ID": PLACEMENT_ID,
        "GAMEFLEET_REVISION_ID": REVISION_ID,
        "GAMEFLEET_REGION": REGION,
        "GAMEFLEET_COMPATIBILITY": COMPATIBILITY,
        # Tripwire: GameFleet selection must bypass the legacy Agones backend.
        "AGONES_FLEET_DATABASE_URL": "intentionally-not-a-dsn",
    }
    if BACKEND == "gamefleet-service":
        # Independent configuration must bypass both the legacy manager and
        # ordinary caller setup. Neither malformed tripwire may be consulted.
        env.update({
            "GAMEFLEET_SERVICE_URL": "http://127.0.0.1:17682",
            "GAMEFLEET_SERVICE_KEY_FILE": "/run/secrets/gamefleet-business-key",
            "GAMEFLEET_SERVICE_ID": SERVICE_ID,
            "GAMEFLEET_SERVICE_APPLICATION_ID": APP_ID,
            "GAMEFLEET_SERVICE_IDENTITY_ISSUER": IDENTITY_ISSUER,
            "GAMEFLEET_SERVICE_REGION": REGION,
            "GAMEFLEET_SERVICE_COMPATIBILITY": COMPATIBILITY,
            "GAMEFLEET_BUSINESS_URL": "not-an-origin",
            "GAMEFLEET_BUSINESS_KEY_FILE": "/ordinary-key-must-not-be-read",
        })
    args = [
        "run", "--detach", "--pull=never", "--platform", "linux/amd64", "--name", name,
        "--network", "container:" + db_container,
        "--mount", f"type=bind,src={config_path},dst=/nakama/data/local.yml,readonly",
        "--mount", f"type=bind,src={key_path},dst=/run/secrets/gamefleet-business-key,readonly",
    ]
    for key, value in env.items():
        args.extend(["--env", key + "=" + value])
    args.extend(["--entrypoint", "/nakama/nakama", RUNTIME_IMAGE,
                 "--config", "/nakama/data/local.yml", "--database.address", dsn])
    docker(args)


def main() -> None:
    global db_container, db_password, REPO, RUNTIME_IMAGE, EVIDENCE_PATH, BACKEND, SYNTHETIC_KEY
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path.cwd(), help="Nakama Agones checkout (default: current directory)")
    parser.add_argument("--image", default=RUNTIME_IMAGE, help="already-built linux/amd64 runtime image tag")
    parser.add_argument("--evidence", type=Path, default=EVIDENCE_PATH, help="evidence JSON output path (contains no bearer credentials)")
    parser.add_argument("--temp-root", type=Path, default=Path(tempfile.gettempdir()), help="temporary build/configuration directory parent")
    parser.add_argument("--backend", choices=("gamefleet", "gamefleet-service"), default="gamefleet", help="explicit isolated bridge mode")
    parser.add_argument("--positive-only", action="store_true", help="run the positive search/match flow and omit startup-denial checks")
    args = parser.parse_args()
    REPO = args.repo.resolve()
    RUNTIME_IMAGE = args.image
    EVIDENCE_PATH = args.evidence
    BACKEND = args.backend
    if BACKEND == "gamefleet-service":
        # Public, deterministic fixture bytes, never a real service credential.
        SYNTHETIC_KEY = "gfsvc_" + base64.urlsafe_b64encode(bytes(range(32))).decode("ascii").rstrip("=")
    if not REPO.is_dir():
        raise SmokeError(f"expected checkout missing: {REPO}")
    suffix = uuid.uuid4().hex[:8]
    prefix = "gf-p4d-" + suffix
    db_container = prefix + "-db"
    network = prefix + "-net"
    fixture_image = prefix + "-fixture:local"
    fixture_container = prefix + "-fixture-ok"
    nakama_container = prefix + "-nakama-ok"
    fail_fixture_container = prefix + "-fixture-deny"
    fail_nakama_container = prefix + "-nakama-deny"
    migration_container = prefix + "-migrate"
    owned_containers = [migration_container, nakama_container, fail_nakama_container, fixture_container, fail_fixture_container, db_container]
    owned_websockets: list[NakamaWebSocket] = []
    temp_root = Path(tempfile.mkdtemp(prefix=prefix + "-", dir=args.temp_root))
    key_path = temp_root / "business.key"
    config_path = temp_root / "nakama.local.yml"
    fixture_source = temp_root / "main.go"
    fixture_binary = temp_root / "gf-p4d-fixture"
    dockerfile = temp_root / "Dockerfile"
    evidence: dict[str, object] = {
        "status": "running",
        "scope": "isolated Nakama plugin, search RPC, matchmaker hook and synthetic binding check; no real GameFleet allocation or Fixed gameplay",
        "runtime_image": RUNTIME_IMAGE,
        "backend": BACKEND,
        "prefix": prefix,
        "checks": [],
        "limits": [
            "Synthetic local Business fixture only; no real GameFleet reservation, host, endpoint, ticket, or allocation was tested.",
            "Temporary PostgreSQL database contains only this smoke's Nakama auth/state and is deleted at cleanup.",
            "Loopback fixture and Nakama API are local-only; no production credentials or service were used.",
        ],
    }
    image_created = False
    network_created = False
    try:
        runtime = image_facts(RUNTIME_IMAGE)
        if runtime["platform"] != "linux/amd64":
            raise SmokeError("runtime image is not linux/amd64")
        evidence["runtime_image_facts"] = runtime
        revision = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, text=True, capture_output=True, check=True).stdout.strip()
        dirty = subprocess.run(["git", "status", "--porcelain"], cwd=REPO, text=True, capture_output=True, check=True).stdout.strip()
        evidence["source_revision"] = revision
        evidence["source_worktree_clean"] = not bool(dirty)
        postgres = image_facts(POSTGRES_IMAGE)
        evidence["postgres_image_facts"] = postgres

        key_path.write_text(SYNTHETIC_KEY, encoding="ascii")
        key_path.chmod(0o400)
        config_path.write_bytes((REPO / "deploy/nakama.local.yml").read_bytes())
        fixture_source.write_text(GO_FIXTURE, encoding="utf-8")
        dockerfile.write_text(
            "FROM scratch\nCOPY gf-p4d-fixture /gf-p4d-fixture\nENTRYPOINT [\"/gf-p4d-fixture\"]\n",
            encoding="utf-8",
        )
        build_env = os.environ.copy()
        build_env.update({"GO111MODULE": "off", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"})
        subprocess.run(["go", "build", "-trimpath", "-o", str(fixture_binary), str(fixture_source)],
                       cwd=temp_root, env=build_env, check=True, capture_output=True, text=True)
        docker(["build", "--pull=false", "--network=none", "--platform", "linux/amd64", "--tag", fixture_image,
                "--file", str(dockerfile), str(temp_root)])
        image_created = True
        evidence["fixture_image_facts"] = image_facts(fixture_image)
        evidence["checks"].append("built static local-only fixture image from scratch; no base image pull")

        docker(["network", "create", network])
        network_created = True
        db_password = secrets.token_urlsafe(24)
        docker([
            "run", "--detach", "--pull=never", "--name", db_container, "--network", network,
            "--network-alias", "postgres", "--publish", "127.0.0.1::7350",
            "--tmpfs", "/var/lib/postgresql/data:rw,nosuid,nodev,size=512m",
            "--env", "POSTGRES_USER=postgres", "--env", "POSTGRES_DB=nakama",
            "--env", "POSTGRES_PASSWORD=" + db_password, POSTGRES_IMAGE,
        ])
        wait_for_db(db_container, time.monotonic() + 60)
        evidence["checks"].append("temporary PostgreSQL ready on tmpfs; database port is not published")
        port = docker(["port", db_container, "7350/tcp"]).stdout.strip().splitlines()[0].rsplit(":", 1)[-1]
        api_url = "http://127.0.0.1:" + port
        evidence["nakama_api"] = "127.0.0.1:<ephemeral> (only this temporary container mapping)"

        dsn = f"postgres:{db_password}@127.0.0.1:5432/nakama?sslmode=disable"
        docker([
            "run", "--rm", "--name", migration_container, "--pull=never", "--platform", "linux/amd64", "--network", "container:" + db_container,
            "--entrypoint", "/nakama/nakama", RUNTIME_IMAGE, "migrate", "up", "--database.address", dsn,
        ], timeout=120)
        evidence["checks"].append("official Nakama 3.41.0 runtime applied migrations to the isolated nakama database")

        start_fixture(fixture_container, fixture_image, "allow")
        start_nakama(nakama_container, config_path, key_path)
        if not wait_health(api_url + "/healthcheck", nakama_container, time.monotonic() + 90):
            evidence["nakama_initial_state"] = container_state(nakama_container)
            evidence["nakama_startup_log_excerpt"] = safe_log_excerpt(logs(nakama_container))
            raise SmokeError("Nakama did not reach /healthcheck after GameFleet initialization")
        ok_logs = logs(nakama_container)
        marker = ("GameFleet service bridge registered; allocation and capacity are owned by GameFleet"
                  if BACKEND == "gamefleet-service" else
                  "GameFleet pilot bridge registered; allocation and capacity are owned by GameFleet")
        if marker not in ok_logs:
            raise SmokeError("Nakama process became healthy without the GameFleet module registration log")
        if "Agones FleetManager registered" in ok_logs or (BACKEND == "gamefleet-service" and "GameFleet pilot bridge registered" in ok_logs):
            raise SmokeError("legacy Agones FleetManager unexpectedly registered in GameFleet mode")
        evidence["checks"].append("live Nakama process loaded agones.so, ran GameFleet scope preflight, and registered the GameFleet bridge")
        evidence["checks"].append("legacy Agones FleetManager registration log absent; invalid legacy DB URL was bypassed")

        device_ids = ["gf-p4d-a-" + uuid.uuid4().hex, "gf-p4d-b-" + uuid.uuid4().hex]
        tokens = []
        for device_id in device_ids:
            token = authenticate(api_url, device_id)
            tokens.append(token)
            status, _ = current_rpc(api_url, token, expect_success=True)
            if status != 200:
                raise SmokeError("synthetic authenticated current RPC did not return HTTP 200")
        ok_fixture_logs = logs(fixture_container)
        expected_scopes = 2 if BACKEND == "gamefleet-service" else 1
        if ok_fixture_logs.count("p4d_fixture_scope_ok") != expected_scopes or ok_fixture_logs.count("p4d_fixture_current_null user=") != 2:
            raise SmokeError("fixture did not observe the exact scope preflights and current-null for both synthetic devices")
        evidence["checks"].append("two synthetic Nakama devices authenticated and each received current=null through gamefleet_current_v1")

        searches = [
            search_begin_rpc(api_url, tokens[0], "smoke-search-begin-a"),
            search_begin_rpc(api_url, tokens[1], "smoke-search-begin-b"),
        ]
        begin_replay = rpc_payload(api_url, tokens[0], "gamefleet_search_begin_v1", {
            "version": SEARCH_VERSION, "compatibility": COMPATIBILITY, "region": REGION,
            "requestId": "smoke-search-begin-a",
        })
        if begin_replay.get("replay") is not True or not isinstance(begin_replay.get("search"), dict) or begin_replay["search"].get("searchId") != searches[0]["searchId"]:
            raise SmokeError("lost-reply begin replay did not recover the same search")
        evidence["checks"].append("search begin RPC creates owned pending searches and exact request replay recovers the same ID")

        ws_a = NakamaWebSocket(api_url, tokens[0])
        ws_b = NakamaWebSocket(api_url, tokens[1])
        owned_websockets.extend([ws_a, ws_b])
        smoke_group = "p4d" + uuid.uuid4().hex
        for ws, search in zip((ws_a, ws_b), searches, strict=True):
            ws.send_json({"cid": "gamefleet-search", "matchmaker_add": {
                "min_count": 2, "max_count": 2, "query": "+properties.smoke_group:" + smoke_group,
                "string_properties": {
                    "smoke_group": smoke_group, "gamefleet_protocol": ROOM_VERSION,
                    "build_hash": COMPATIBILITY, "region": REGION,
                    "gamefleet_search_id": search["searchId"],
                },
            }})
        wait_matchmaker_result([ws_a, ws_b], matched=True)
        fixture_deadline = time.monotonic() + 15
        while time.monotonic() < fixture_deadline and "p4d_fixture_search_match replay=false" not in logs(fixture_container):
            time.sleep(0.25)
        ok_fixture_logs = logs(fixture_container)
        if ok_fixture_logs.count("p4d_fixture_search_status user=") < 2 or "p4d_fixture_search_match replay=false" not in ok_fixture_logs:
            raise SmokeError("MatchmakerAdd did not check owned pending searches and bind the matched pair")
        evidence["checks"].append("two websocket MatchmakerAdd requests passed owned-pending before status and the matched hook bound one exact pair")

        bound_searches = [
            search_participant_rpc(api_url, tokens[0], "gamefleet_search_status_v1", searches[0]["searchId"]),
            search_participant_rpc(api_url, tokens[1], "gamefleet_search_status_v1", searches[1]["searchId"]),
        ]
        allocation_id = bound_searches[0].get("search", {}).get("allocationId") if isinstance(bound_searches[0].get("search"), dict) else None
        if not isinstance(allocation_id, str) or not allocation_id or any(
            not isinstance(result.get("search"), dict) or result["search"].get("state") != "bound" or result["search"].get("allocationId") != allocation_id
            for result in bound_searches
        ):
            raise SmokeError("search status did not recover the exact matched binding for both users")
        bound_cancel = search_participant_rpc(api_url, tokens[0], "gamefleet_search_cancel_v1", searches[0]["searchId"])
        if not isinstance(bound_cancel.get("search"), dict) or bound_cancel["search"].get("state") != "bound" or bound_cancel["search"].get("allocationId") != allocation_id:
            raise SmokeError("cancel of a bound search hid its existing room")
        for token in tokens:
            status, _ = current_rpc(api_url, token, expect_success=True, expected_allocation=allocation_id)
            if status != 200:
                raise SmokeError("bound room was not retained by the original Current endpoint")
        evidence["checks"].append("bound search status and cancel preserve one reserved room visible through Current")

        expect_rpc_denied(api_url, tokens[1], "gamefleet_search_status_v1", {
            "version": SEARCH_VERSION, "compatibility": COMPATIBILITY, "region": REGION,
            "searchId": searches[0]["searchId"],
        })
        token_c = authenticate(api_url, "gf-p4d-c-" + uuid.uuid4().hex)
        third_search = search_begin_rpc(api_url, token_c, "smoke-search-cancel-c")
        expect_rpc_denied(api_url, tokens[1], "gamefleet_search_cancel_v1", {
            "version": SEARCH_VERSION, "compatibility": COMPATIBILITY, "region": REGION,
            "searchId": third_search["searchId"],
        })
        cancelled = search_participant_rpc(api_url, token_c, "gamefleet_search_cancel_v1", third_search["searchId"])
        if not isinstance(cancelled.get("search"), dict) or cancelled["search"].get("state") != "cancelled":
            raise SmokeError("search cancel RPC did not persist its terminal state")
        ws_c = NakamaWebSocket(api_url, token_c)
        owned_websockets.append(ws_c)
        cancelled_group = "p4d" + uuid.uuid4().hex
        ws_c.send_json({"cid": "gamefleet-cancelled-search", "matchmaker_add": {
            "min_count": 2, "max_count": 2, "query": "+properties.smoke_group:" + cancelled_group,
            "string_properties": {
                "smoke_group": cancelled_group, "gamefleet_protocol": ROOM_VERSION,
                "build_hash": COMPATIBILITY, "region": REGION,
                "gamefleet_search_id": third_search["searchId"],
            },
        }})
        wait_matchmaker_result([ws_c], matched=False)
        ok_fixture_logs = logs(fixture_container)
        if "p4d_fixture_search_owner_denied" not in ok_fixture_logs or "p4d_fixture_search_status user=" not in ok_fixture_logs or "state=cancelled" not in ok_fixture_logs:
            raise SmokeError("cross-user search access or queueing a cancelled search was not rejected")
        evidence["checks"].append("cross-participant status/cancel and a late MatchmakerAdd for a cancelled search are rejected")
        if "p4d_fixture_unexpected" in ok_fixture_logs:
            raise SmokeError("bridge called a route outside its selected backend")
        if BACKEND == "gamefleet-service":
            route_counts = {
                "mapped_status": ok_fixture_logs.count("p4d_fixture_search_route use=mapped operation=status"),
                "history_status": ok_fixture_logs.count("p4d_fixture_search_route use=history operation=status"),
                "history_cancel": ok_fixture_logs.count("p4d_fixture_search_route use=history operation=cancel"),
            }
            if min(route_counts.values()) < 2 or "p4d_fixture_search_route use=ordinary" in ok_fixture_logs:
                raise SmokeError("service matching and player History did not use distinct routes and protocols")
            evidence["service_route_counts"] = route_counts
            evidence["checks"].append("independent gfsvc mode uses mapped search admission and separate History status/cancel; no ordinary caller routes")
        evidence["synthetic_devices"] = 3
        evidence["business_fixture_requests"] = {
            "scope_preflights": expected_scopes,
            "current_null_before_match": 2,
            "search_begin_calls_including_replay": ok_fixture_logs.count("p4d_fixture_search_begin user="),
            "search_status_calls": ok_fixture_logs.count("p4d_fixture_search_status user="),
            "search_cancel_calls": ok_fixture_logs.count("p4d_fixture_search_cancel user="),
            "search_match_commits": ok_fixture_logs.count("p4d_fixture_search_match replay=false"),
            "foreign_search_denials": ok_fixture_logs.count("p4d_fixture_search_owner_denied"),
        }
        print("LIVE POSITIVE CHECK COMPLETE: plugin registered; authenticated searches, matchmaker admission, matched binding, and cancellation fences passed", flush=True)
        if args.positive_only:
            evidence["status"] = "passed-positive-only"
            evidence["scope_failure"] = "not run (--positive-only)"
            return

        docker(["stop", "--time", "10", nakama_container], check=False, timeout=20)
        docker(["stop", "--time", "5", fixture_container], check=False, timeout=15)
        failure_cases = [("deny", "p4d_fixture_scope_denied",
                          "GameFleet service search scope preflight failed" if BACKEND == "gamefleet-service" else
                          "GameFleet business scope preflight failed", "HTTP 403")]
        if BACKEND == "gamefleet-service":
            failure_cases.append(("history-read-only", "p4d_fixture_scope_read_only",
                                  "GameFleet history service scope preflight failed", "HTTP 200; required History operations absent"))
        evidence["scope_failures"] = []
        for failure_mode, fixture_marker, failure_marker, fixture_status in failure_cases:
            start_fixture(fail_fixture_container, fixture_image, failure_mode)
            start_nakama(fail_nakama_container, config_path, key_path)
            fail_state, fail_code = "running", 0
            deadline = time.monotonic() + 45
            while time.monotonic() < deadline:
                fail_state, fail_code = container_state(fail_nakama_container)
                if fail_state != "running":
                    break
                time.sleep(0.5)
            fail_logs = logs(fail_nakama_container)
            fixture_fail_logs = logs(fail_fixture_container)
            if fixture_marker not in fixture_fail_logs:
                raise SmokeError("scope-denial fixture was not called")
            if marker in fail_logs or "Agones FleetManager registered" in fail_logs or "GameFleet pilot bridge registered" in fail_logs:
                raise SmokeError("scope-denial path registered a fleet backend")
            if failure_marker not in fail_logs:
                raise SmokeError("Nakama logs did not report the expected scope preflight failure")
            if failure_mode == "history-read-only" and fixture_fail_logs.count("p4d_fixture_scope_read_only") != 2:
                raise SmokeError("read-only scope did not pass Search before failing the independent History preflight")
            if fail_state != "exited" or fail_code == 0:
                raise SmokeError(f"Nakama did not abort with a nonzero exit after scope preflight failure (state={fail_state}, code={fail_code})")
            evidence["checks"].append(f"{failure_mode} scope made Nakama exit nonzero before readiness, without backend fallback")
            evidence["scope_failures"].append({"mode": failure_mode, "fixture_status": fixture_status,
                                                "nakama_container_state": fail_state, "exit_code": fail_code})
            docker(["rm", "--force", fail_nakama_container, fail_fixture_container], check=False, timeout=20)
        evidence["status"] = "passed"
    except Exception as exc:
        evidence["status"] = "failed"
        evidence["failure"] = str(exc)
        raise
    finally:
        for ws in owned_websockets:
            ws.close()
        for name in owned_containers:
            docker(["rm", "--force", name], check=False, timeout=20)
        if network_created:
            docker(["network", "rm", network], check=False, timeout=20)
        if image_created:
            docker(["image", "rm", "--force", fixture_image], check=False, timeout=20)
        shutil.rmtree(temp_root, ignore_errors=True)
        EVIDENCE_PATH.write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        print("evidence:", EVIDENCE_PATH)
        print("status:", evidence["status"])
        for check in evidence["checks"]:
            print("PASS:", check)
        if "failure" in evidence:
            print("FAIL:", evidence["failure"])


db_container = ""

if __name__ == "__main__":
    main()
