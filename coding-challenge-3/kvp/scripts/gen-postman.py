"""Generates the Postman collection: one happy-path scenario plus one folder per
acceptance criterion, with one or two checks per request.

Usage: python3 scripts/gen-postman.py postman/ridehail.postman_collection.json
"""
import json
import sys

OUT = sys.argv[1]


# ---------- building blocks ----------

def req(name, method, path, body=None, headers=None, tests="", pre="", desc=""):
    h = [{"key": "Content-Type", "value": "application/json"}] if body is not None else []
    h += [{"key": k, "value": v} for k, v in (headers or {}).items()]
    item = {
        "name": name,
        "request": {
            "method": method,
            "header": h,
            "url": {"raw": "{{baseUrl}}" + path, "host": ["{{baseUrl}}"], "path": path.strip("/").split("/")},
        },
        "event": [],
    }
    if desc:
        item["request"]["description"] = desc
    if body is not None:
        item["request"]["body"] = {"mode": "raw", "raw": body, "options": {"raw": {"language": "json"}}}
    for listen, script in (("prerequest", pre), ("test", tests)):
        if script:
            item["event"].append({"listen": listen, "script": {"type": "text/javascript", "exec": script.strip("\n").split("\n")}})
    return item


def folder(name, desc, items):
    return {"name": name, "description": desc, "item": items}


def setup(drivers=()):
    """Pre-request: a random, isolated spot on the map and fixture drivers there.

    drivers: (variable, metres north of the pickup, status). Drivers are created
    through driver-service's fixture API (not exposed by the gateway).
    """
    seeds = ",\n  ".join(f"['{v}', {m}, '{st}']" for v, m, st in drivers)
    return f"""
// Setup: a random spot on the map so folders never interfere, plus this folder's drivers.
const run = Date.now().toString(36) + Math.floor(Math.random() * 1e4).toString(36);
const lat = -50 + Math.random() * 100, lng = -170 + Math.random() * 340;
pm.collectionVariables.set('run', run);
pm.collectionVariables.set('lat', lat.toFixed(6));
pm.collectionVariables.set('lng', lng.toFixed(6));
pm.collectionVariables.set('destLat', (lat + 0.05).toFixed(6));
const drivers = [
  {seeds}
];
drivers.forEach(([v, metres, status]) => {{
  const id = 'PM-' + v.slice(-1) + '-' + run;  // PM-A-… < PM-B-… (tie-break order)
  pm.collectionVariables.set(v, id);
  pm.sendRequest({{
    url: pm.variables.replaceIn('{{{{driverServiceUrl}}}}/internal/drivers/') + id,
    method: 'PUT',
    header: {{ 'Content-Type': 'application/json' }},
    body: {{ mode: 'raw', raw: JSON.stringify({{ status, latitude: lat + metres / 111195, longitude: lng }}) }}
  }}, (err, res) => {{ if (err || res.code !== 200) console.error('fixture driver failed', id, err || res.code); }});
}});
"""


def wait(path, cond, what, max_s=10):
    """Pre-request: dispatch is asynchronous, so wait until GET path satisfies cond(b)."""
    return f"""
// Dispatch is asynchronous: wait (max {max_s} s) until {what}.
const url = pm.variables.replaceIn('{{{{baseUrl}}}}{path}');
(function poll(n) {{
  pm.sendRequest(url, (err, res) => {{
    let b = {{}};
    try {{ b = res.json(); }} catch (e) {{}}
    if (!err && ({cond})) return;
    if (n > 0) setTimeout(() => poll(n - 1), 250);
  }});
}})({max_s * 4});
"""


def check(code, field=None, value=None, label=None):
    lines = [f"pm.test('status {code}', () => pm.response.to.have.status({code}));"]
    if field:
        v = value if isinstance(value, str) and value.startswith("pm.") else json.dumps(value)
        lines.append(f"pm.test('{label or field + ' = ' + str(value)}', () => pm.expect(pm.response.json().{field}).to.eql({v}));")
    return "\n".join(lines)


def var(name):
    return f"pm.collectionVariables.get('{name}')"


RIDE = """{
  "passengerId": "P001",
  "pickup": { "latitude": {{lat}}, "longitude": {{lng}} },
  "destination": { "latitude": {{destLat}}, "longitude": {{lng}} }
}"""


def create(name="Passenger requests a ride", save="rideId", pre="", key=None):
    return req(name, "POST", "/rides", body=RIDE, headers={"Idempotency-Key": key or "{{run}}-" + save}, pre=pre,
               tests=check(201, "status", "REQUESTED") + f"\npm.collectionVariables.set('{save}', pm.response.json().rideId);")


def ride_is(name, status, driver=None, ride="rideId", max_s=10):
    want = var(driver) if driver else "null"
    cond = f"b.status === '{status}' && b.assignedDriverId === {want}"
    return req(name, "GET", "/rides/{{" + ride + "}}", pre=wait("/rides/{{" + ride + "}}", cond, f"the ride is {status}", max_s),
               tests=check(200, "status", status) +
               f"\npm.test('assignedDriverId', () => pm.expect(pm.response.json().assignedDriverId).to.eql({want}));")


def driver_is(name, driver, status):
    return req(name, "GET", "/drivers/{{" + driver + "}}",
               pre=wait("/drivers/{{" + driver + "}}", f"b.status === '{status}'", f"the driver is {status}"),
               tests=check(200, "status", status))


def respond(name, action, driver, code=200, field=None, value=None, ride="rideId"):
    return req(name, "POST", "/rides/{{" + ride + "}}/offers/{{" + driver + "}}/" + action, tests=check(code, field, value))


def ride_action(name, action, code=200, field=None, value=None, ride="rideId"):
    return req(name, "POST", "/rides/{{" + ride + "}}/" + action, tests=check(code, field, value))


# ---------- folders ----------

items = []

items.append(folder("Scenario · Happy path", (
    "AC-01, AC-04, AC-05, AC-06, AC-10. Request a ride → the nearest driver gets an offer → accepts → ride completed → "
    "driver available again. Two drivers: A ~300 m and B ~1.5 km from the pickup."), [
    create(pre=setup([("driverA", 300, "AVAILABLE"), ("driverB", 1500, "AVAILABLE")])),
    ride_is("Nearest driver A gets the offer (DRIVER_ASSIGNED)", "DRIVER_ASSIGNED", "driverA"),
    req("Driver A sees the pending offer", "GET", "/drivers/{{driverA}}/offer", tests=check(200, "status", "PENDING")),
    respond("Driver A accepts", "accept", "driverA", field="status", value="ACCEPTED"),
    driver_is("Driver A is BUSY", "driverA", "BUSY"),
    ride_action("Ride is completed", "complete", field="status", value="COMPLETED"),
    driver_is("Driver A is AVAILABLE again", "driverA", "AVAILABLE"),
    req("History: every status change is recorded", "GET", "/rides/{{rideId}}/history", tests=check(200) + """
pm.test('REQUESTED → MATCHING → DRIVER_ASSIGNED → DRIVER_ACCEPTED → COMPLETED', () =>
  pm.expect(pm.response.json().transitions.map(t => t.to)).to.eql(['REQUESTED', 'MATCHING', 'DRIVER_ASSIGNED', 'DRIVER_ACCEPTED', 'COMPLETED']));"""),
    req("Dispatch view: the saga that drove it (events and commands)", "GET", "/rides/{{rideId}}/dispatch",
        tests=check(200, "saga.status", "COMPLETED")),
]))

items.append(folder("AC-01 · Create ride", (
    "A ride starts in REQUESTED with a unique id; invalid coordinates, an unknown passenger and a missing "
    "Idempotency-Key are rejected before any matching."), [
    create(pre=setup()),
    req("Invalid coordinates → 422", "POST", "/rides", headers={"Idempotency-Key": "{{run}}-bad-coords"},
        body='{ "passengerId": "P001", "pickup": { "latitude": 91, "longitude": 106.7 }, "destination": { "latitude": 10.8, "longitude": 106.6 } }',
        tests=check(422, "code", "INVALID_COORDINATES")),
    req("Unknown passenger → 422", "POST", "/rides", headers={"Idempotency-Key": "{{run}}-bad-passenger"},
        body='{ "passengerId": "P999", "pickup": { "latitude": 10.7, "longitude": 106.7 }, "destination": { "latitude": 10.8, "longitude": 106.6 } }',
        tests=check(422, "code", "UNKNOWN_PASSENGER")),
    req("Missing Idempotency-Key → 400", "POST", "/rides", body=RIDE, tests=check(400, "code", "IDEMPOTENCY_KEY_REQUIRED")),
    ride_action("Cleanup: cancel the ride", "cancel"),
]))

items.append(folder("AC-02 · AC-03 · Driver availability and location", (
    "Drivers switch OFFLINE ⇄ AVAILABLE; other statuses cannot be set through the API. Location updates are validated "
    "and the latest one is stored (its version grows)."), [
    req("OFFLINE → AVAILABLE", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "AVAILABLE" }',
        pre=setup([("driverA", 100, "OFFLINE")]), tests=check(200, "status", "AVAILABLE")),
    req("AVAILABLE → OFFLINE", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "OFFLINE" }',
        tests=check(200, "status", "OFFLINE")),
    req("BUSY cannot be set through the API → 422", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "BUSY" }',
        tests=check(422, "code", "INVALID_STATUS")),
    req("Update location", "PUT", "/drivers/{{driverA}}/location", body='{ "latitude": {{lat}}, "longitude": {{lng}} }',
        tests=check(200, "latitude", "pm.collectionVariables.get('lat') * 1", "latitude is the latest one")),
    req("Invalid location → 422", "PUT", "/drivers/{{driverA}}/location", body='{ "latitude": 95, "longitude": 106.7 }',
        tests=check(422, "code", "INVALID_COORDINATES")),
]))

items.append(folder("AC-04 · Matching: nearest, ties, radius", (
    "Drivers A and B are both ~500 m away (a tie) and C is ~2.5 km away (outside the 2 km radius). "
    "The tie goes to the smaller driverId (A)."), [
    create(pre=setup([("driverA", 500, "AVAILABLE"), ("driverB", 500, "AVAILABLE"), ("driverC", 2500, "AVAILABLE")])),
    ride_is("Tie → smaller driverId A gets the offer", "DRIVER_ASSIGNED", "driverA"),
    req("Driver C (outside the radius) has no offer → 204", "GET", "/drivers/{{driverC}}/offer", tests=check(204)),
    ride_action("Cleanup: cancel the ride", "cancel"),
]))

items.append(folder("AC-05 · AC-07 · Reject → rematch → exclusion", (
    "Only the offered driver can answer. A rejection excludes that driver for this ride and the next nearest driver "
    "gets the offer. With nobody left, the ride stays MATCHING."), [
    create(pre=setup([("driverA", 200, "AVAILABLE"), ("driverB", 900, "AVAILABLE")])),
    ride_is("Offer goes to A (nearest)", "DRIVER_ASSIGNED", "driverA"),
    respond("Another driver cannot answer → 404", "accept", "driverB", 404, "code", "OFFER_NOT_FOUND"),
    respond("A rejects", "reject", "driverA", field="status", value="REJECTED"),
    ride_is("Rematched to B", "DRIVER_ASSIGNED", "driverB"),
    respond("A cannot accept any more → 409", "accept", "driverA", 409, "code", "OFFER_NOT_CURRENT"),
    respond("B rejects too", "reject", "driverB"),
    ride_is("Nobody left (A is available but excluded): ride stays MATCHING", "MATCHING", None),
    ride_action("Cleanup: cancel the ride", "cancel"),
]))

items.append(folder("AC-05 · Offer timeout → rematch (slow, ~35 s)", (
    "No answer within OFFER_TIMEOUT (default 30 s) counts as a rejection. Tip: `OFFER_TIMEOUT=5s make up` makes it fast."), [
    create(pre=setup([("driverA", 100, "AVAILABLE"), ("driverB", 600, "AVAILABLE")])),
    ride_is("Offer goes to A", "DRIVER_ASSIGNED", "driverA"),
    ride_is("A does not answer: after the timeout B gets the offer", "DRIVER_ASSIGNED", "driverB", max_s=45),
    respond("A accepts too late → 409", "accept", "driverA", 409),
    ride_action("Cleanup: cancel the ride", "cancel"),
]))

items.append(folder("AC-06 · Ride state machine", (
    "Invalid transitions are rejected with 409; repeating a valid transition is a harmless no-op. No drivers in this area, "
    "so the ride waits in MATCHING."), [
    create(pre=setup()),
    ride_is("No driver nearby: ride stays MATCHING", "MATCHING", None),
    ride_action("Complete while MATCHING → 409", "complete", 409, "code", "INVALID_TRANSITION"),
    ride_action("Cancel → CANCELLED", "cancel", field="status", value="CANCELLED"),
    ride_action("Cancel again → 200, no change", "cancel", field="status", value="CANCELLED"),
]))

items.append(folder("AC-08 · Idempotency", (
    "A retried request with the same Idempotency-Key returns the original ride; a repeated accept does not change "
    "anything a second time."), [
    create(pre=setup([("driverA", 100, "AVAILABLE")]), key="{{run}}-idem"),
    req("Same key + same body → the original ride", "POST", "/rides", body=RIDE, headers={"Idempotency-Key": "{{run}}-idem"},
        tests=check(201, "rideId", "pm.collectionVariables.get('rideId')", "same rideId") +
        "\npm.test('Idempotent-Replayed header', () => pm.response.to.have.header('Idempotent-Replayed', 'true'));"),
    req("Same key + different body → 422", "POST", "/rides", headers={"Idempotency-Key": "{{run}}-idem"},
        body=RIDE.replace('"P001"', '"P002"'), tests=check(422, "code", "IDEMPOTENCY_KEY_REUSED")),
    ride_is("Offer goes to A", "DRIVER_ASSIGNED", "driverA"),
    respond("A accepts", "accept", "driverA", field="status", value="ACCEPTED"),
    req("A accepts again → 200, replayed, nothing changes", "POST", "/rides/{{rideId}}/offers/{{driverA}}/accept",
        tests=check(200, "status", "ACCEPTED") +
        "\npm.test('Idempotent-Replayed header', () => pm.response.to.have.header('Idempotent-Replayed', 'true'));"),
    ride_is("Ride is DRIVER_ACCEPTED (a valid state, accepted once)", "DRIVER_ACCEPTED", "driverA"),
    ride_action("Cleanup: complete the ride", "complete"),
]))

race = """
// Both rides race for the only driver. Wait until they settle, then record the winner.
const base = pm.variables.replaceIn('{{baseUrl}}/rides/');
const ids = [pm.collectionVariables.get('ride1'), pm.collectionVariables.get('ride2')];
(function poll(n) {
  pm.sendRequest(base + ids[0], (e1, r1) => {
    pm.sendRequest(base + ids[1], (e2, r2) => {
      const a = r1.json(), b = r2.json();
      if ([a.status, b.status].sort().join() === 'DRIVER_ASSIGNED,MATCHING') {
        const winner = a.status === 'DRIVER_ASSIGNED' ? ids[0] : ids[1];
        pm.collectionVariables.set('winner', winner);
        pm.collectionVariables.set('loser', winner === ids[0] ? ids[1] : ids[0]);
        return;
      }
      if (n > 0) setTimeout(() => poll(n - 1), 250);
    });
  });
})(40);
"""
items.append(folder("AC-09 · Two rides, one driver", (
    "Two rides compete for the only nearby driver: exactly one gets the offer, the other waits in MATCHING. When the "
    "winner cancels, the driver is released (saga compensation) and offered to the waiting ride."), [
    create("Ride 1 requested", save="ride1", pre=setup([("driverA", 150, "AVAILABLE")])),
    create("Ride 2 requested right after", save="ride2"),
    req("Driver A has exactly one offer, for the winning ride", "GET", "/drivers/{{driverA}}/offer", pre=race,
        tests=check(200, "rideId", "pm.collectionVariables.get('winner')", "offer is for the winner")),
    ride_action("Winner cancels", "cancel", ride="winner"),
    ride_is("A is released and offered to the waiting ride", "DRIVER_ASSIGNED", "driverA", ride="loser", max_s=15),
    ride_action("Cleanup: cancel the other ride", "cancel", ride="loser"),
]))

collection = {
    "info": {
        "name": "Ride-Hailing Dispatch (KVP)",
        "description": (
            "Acceptance-criteria scenarios for Coding Challenge #3. Start the stack first (`make up` in coding-challenge-3/kvp).\n\n"
            "* **Run** the collection with the Runner (keep the order), or one folder at a time. "
            "From the terminal: `make postman`.\n"
            "* **Folders:** one happy-path scenario, then one folder per acceptance criterion (AC-01 … AC-09). "
            "AC-10 (event-driven dispatch) is what every folder relies on: each result is reached asynchronously.\n"
            "* **Self-contained:** each folder's first request picks a random spot on the map and creates its own drivers "
            "through the driver-service fixture API (`{{driverServiceUrl}}/internal/drivers/...`).\n"
            "* **Asynchronous:** requests that check a result first wait in their pre-request script until it is reached.\n"
            "* **Variables:** `baseUrl` (gateway, http://localhost:8080) and `driverServiceUrl` (http://localhost:8082).\n"
            "* **API reference:** `api/openapi.yaml` (REST). gRPC v2 is not in this collection: see `api/proto` and `make grpc-offers`."),
        "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
    },
    "variable": [
        {"key": "baseUrl", "value": "http://localhost:8080"},
        {"key": "driverServiceUrl", "value": "http://localhost:8082"},
    ],
    "item": items,
}
with open(OUT, "w") as f:
    json.dump(collection, f, indent=2, ensure_ascii=False)
    f.write("\n")
n = sum(len(f["item"]) for f in items)
t = sum(line.count("pm.test(") for f in items for i in f["item"] for e in i["event"]
        if e["listen"] == "test" for line in e["script"]["exec"])
print(f"{len(items)} folders, {n} requests, {t} checks -> {OUT}")
