"""LOCAL PROTOCOL FIXTURE ONLY. Not FishNet, Unity, physics or a production server."""
import base64
import hashlib
import hmac
import json
import os
import socket
import time
import urllib.request
import uuid

MAX_BODY = 131072
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise ValueError("redirect refused")
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
def call(url, body=None, token=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), headers=headers)
    with opener.open(req, timeout=2) as res:
        payload = res.read(MAX_BODY + 1)
        if len(payload) > MAX_BODY:
            raise ValueError("response too large")
        return json.loads(payload or b"{}")
def decode(s):
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))
def required(s):
    return os.environ["AGONES_FLEET_" + s]

wid, build, control = required("WORKER_ID"), required("BUILD_HASH"), required("CONTROL_URL").rstrip("/")
boot = str(uuid.uuid4())
admission = decode(required("ADMISSION_KEY"))
max_rooms = int(required("MAX_ROOMS"))
sdk = "http://127.0.0.1:" + os.environ.get("AGONES_SDK_HTTP_PORT", "9358")
api = control + "/agones/fleet/v1/agent/"
sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.bind(("0.0.0.0", int(required("GAME_PORT"))))
sock.setblocking(False)
rooms, outcomes, cancelled, nonces, links = {}, {}, set(), {}, {}
sequence, pending, token, uid = 0, None, None, None
last_hb, next_tick, sdk_ready, draining, empty_ack = 0, 0, False, False, False
results_until = 0
confirmed_outcomes = set()

# A single event loop makes room/seat/nonce consumption atomic in this fixture.
def packet(data, addr):
    global results_until
    msg = json.loads(data)
    op = msg.get("op")
    if op == "join":
        payload, signature = msg["ticket"].split(".")
        expected = hmac.new(admission, payload.encode("ascii"), hashlib.sha256).digest()
        if not hmac.compare_digest(expected, decode(signature)):
            raise ValueError("signature")
        c = json.loads(decode(payload)); now = time.time()
        room = rooms.get(c["room_id"])
        if not sdk_ready or now-last_hb > 8 or c["schema_version"] != 1 or c["worker_id"] != wid or c["boot_id"] != boot:
            raise ValueError("identity/readiness")
        if room is None or room["state"] == "closed" or c["allocation_id"] != room["allocation_id"] or c["epoch"] != room["epoch"]:
            raise ValueError("room")
        if c["seat"] not in (0, 1) or room["roster"][c["seat"]] != c["user_id"] or not now < c["exp"] <= now+60:
            raise ValueError("seat/expiry")
        if c["nonce"] in nonces or len(nonces) >= 2048:
            raise ValueError("nonce")
        if draining and not c.get("resume"):
            raise ValueError("draining")
        nonces[c["nonce"]] = c["exp"]
        links[addr] = (c["room_id"], c["user_id"])
        room["players"][c["user_id"]] = addr
        room["state"] = "playing" if len(room["players"]) == 2 else "waiting_players"
        return {"ok": True, "room_id": c["room_id"], "seat": c["seat"]}
    if addr not in links:
        raise ValueError("connection")
    rid, user = links[addr]; room = rooms.get(rid)
    if room is None or room["players"].get(user) != addr:
        raise ValueError("connection")
    if op == "ping":
        return {"ok": True}
    if op == "disconnect":
        room["players"].pop(user); links.pop(addr)
        return {"ok": True}
    if op == "close":
        room["state"] = "closed"; room["players"] = {}
        results_until = max(results_until, time.time()+5)
        return {"ok": True}
    raise ValueError("operation")

while True:
    now = time.time()
    if now >= next_tick:
        next_tick = now + 1
        try:
            call(sdk+"/health", {})
            gs = call(sdk+"/gameserver")
            meta = gs.get("object_meta", gs.get("objectMeta", {}))
            if meta.get("name") != "nag-"+wid or not meta.get("uid") or uid and uid != meta["uid"]:
                raise ValueError("SDK identity")
            uid = meta["uid"]
            state = gs.get("status", {}).get("state")
            if state in ("Scheduled", "Starting", "RequestReady"):
                call(sdk+"/ready", {})
            elif state == "Ready":
                call(sdk+"/allocate", {})
            sdk_ready = state == "Allocated"
            if token is None:
                response = call(api+"bootstrap", {"worker_id":wid,"boot_id":boot,"build_hash":build,"bootstrap_token":required("BOOTSTRAP_TOKEN")})
                token = response["agent_token"]
            if pending is None:
                sequence += 1
                pending = {"worker_id":wid,"boot_id":boot,"sequence":sequence,"ready":sdk_ready and not draining,
                    "rooms":[{"room_id":rid,"state":v["state"],"user_ids":list(v["players"])} for rid,v in rooms.items()],
                    "metrics":{"pending_results":int(now<results_until)},"command_results":list(outcomes.values())}
            response = call(api+"heartbeat", pending, token)
            last_hb = time.time()
            confirmed_outcomes.update(item["command_id"] for item in pending["command_results"])
            empty_ack = not pending["rooms"] and not pending["metrics"]["pending_results"]
            for room in pending["rooms"]:
                if room["state"] == "closed":
                    rooms.pop(room["room_id"], None)
            pending = None
            for c in response.get("commands", []):
                if c["command_id"] in outcomes:
                    continue
                ok = True
                if c["type"] == "prepare_room":
                    ok = not draining and c["expires_at"] > time.time() and c["room_id"] not in cancelled and len(rooms)<max_rooms
                    if ok:
                        rooms[c["room_id"]] = {"allocation_id":c["allocation_id"],"epoch":c["epoch"],"roster":c["user_ids"],"players":{},"state":"waiting_players"}
                elif c["type"] == "cancel_room":
                    cancelled.add(c["room_id"]); rooms.pop(c["room_id"], None)
                elif c["type"] == "drain":
                    draining = True
                else:
                    ok = False
                outcomes[c["command_id"]] = {"command_id":c["command_id"],"success":ok}
            draining = draining or response.get("draining", False)
            if draining and not rooms and empty_ack and all(k in confirmed_outcomes for k in outcomes) and time.time() >= results_until:
                call(sdk+"/shutdown", {})
                print("FIXTURE_DRAINED", flush=True)
                break
            # This fixture is bounded and exits instead of evicting live safety state.
            if len(outcomes)>1000 or len(cancelled)>1000:
                raise SystemExit("fixture command bound reached")
        except Exception as exc:
            sdk_ready = False
            # Avoid URLs, tokens, body contents or raw exceptions in logs.
            print("FIXTURE_CONTROL_RETRY type="+type(exc).__name__, flush=True)
    nonces = {key:expiry for key,expiry in nonces.items() if expiry>now}
    for _ in range(64):
        try:
            data, addr = sock.recvfrom(8193)
        except BlockingIOError:
            break
        try:
            if len(data)>8192:
                raise ValueError("oversized")
            reply = packet(data, addr)
        except Exception:
            reply = {"ok":False}
        sock.sendto(json.dumps(reply).encode(), addr)
    time.sleep(0.01)
