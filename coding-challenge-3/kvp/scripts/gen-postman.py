"""Generates postman/ridehail.postman_collection.json. Usage: python3 scripts/gen-postman.py postman/ridehail.postman_collection.json"""
import json, sys

OUT = sys.argv[1]

def js(s):
    return [l for l in s.strip("\n").split("\n")]

def req(name, method, path, body=None, headers=None, tests="", pre="", base="{{baseUrl}}", desc=""):
    h = [{"key": "Content-Type", "value": "application/json"}] if body is not None else []
    for k, v in (headers or {}).items():
        h.append({"key": k, "value": v})
    url = base + path
    raw_path = path.split("?")[0].strip("/").split("/")
    item = {
        "name": name,
        "request": {
            "method": method,
            "header": h,
            "url": {"raw": url, "host": [base], "path": raw_path},
        },
        "event": [],
    }
    if desc:
        item["request"]["description"] = desc
    if body is not None:
        item["request"]["body"] = {"mode": "raw", "raw": body if isinstance(body, str) else json.dumps(body, indent=2),
                                   "options": {"raw": {"language": "json"}}}
    if pre:
        item["event"].append({"listen": "prerequest", "script": {"type": "text/javascript", "exec": js(pre)}})
    if tests:
        item["event"].append({"listen": "test", "script": {"type": "text/javascript", "exec": js(tests)}})
    return item

def folder(name, desc, items):
    return {"name": name, "description": desc, "item": items}

# ---------- reusable script snippets ----------

def poll(url, cond, what, max_s=10):
    """Pre-request: wait until GET url satisfies cond(body) (dispatch is asynchronous)."""
    tries = max_s * 4
    return f"""
// Dispatch is asynchronous: wait (max {max_s} s) until {what}.
const url = pm.variables.replaceIn('{url}');
const ok = (b) => {cond};
(function poll(n) {{
  pm.sendRequest(url, (err, res) => {{
    let body = {{}};
    try {{ body = res.json(); }} catch (e) {{}}
    if (!err && ok(body)) return;
    if (n > 0) setTimeout(() => poll(n - 1), 250);
    else console.warn('timed out waiting until {what}', body);
  }});
}})({tries});
"""

def area(drivers):
    """Pre-request for a scenario's first request: random isolated area + driver ids/positions."""
    lines = ["// Each scenario uses its own random spot on the map so scenarios never interfere.",
             "const run = Date.now().toString(36);",
             "const lat = -50 + Math.random() * 100, lng = -170 + Math.random() * 340;",
             "pm.collectionVariables.set('run', run);",
             "pm.collectionVariables.set('lat', lat.toFixed(6));",
             "pm.collectionVariables.set('lng', lng.toFixed(6));",
             "pm.collectionVariables.set('destLat', (lat + 0.05).toFixed(6));",
             "// a point ~m metres north of the pickup (1° latitude ≈ 111.2 km)",
             "const north = (m) => (lat + m / 111195).toFixed(6);"]
    for var, metres in drivers:
        lines.append(f"pm.collectionVariables.set('{var}', 'PM-{var[-1].upper()}-' + run);  // ~{metres} m")
        lines.append(f"pm.collectionVariables.set('{var}Lat', north({metres}));")
    return "\n".join(lines)

def status(code):
    return f"pm.test('status {code}', () => pm.response.to.have.status({code}));"

def field(path, value, label=None):
    v = json.dumps(value) if not isinstance(value, str) or not value.startswith("pm.") else value
    return f"pm.test('{label or path + ' = ' + str(value)}', () => pm.expect(pm.response.json().{path}).to.eql({v}));"

def code(c):
    return f"pm.test('error code {c}', () => pm.expect(pm.response.json().code).to.eql('{c}'));"

def var(name):
    return f"pm.collectionVariables.get('{name}')"

RIDE_BODY = """{
  "passengerId": "P001",
  "pickup": { "latitude": {{lat}}, "longitude": {{lng}} },
  "destination": { "latitude": {{destLat}}, "longitude": {{lng}} }
}"""

def seed(name, v, status_="AVAILABLE"):
    return req(f"Setup: driver {name} {status_} (fixture API)", "PUT", "/internal/drivers/{{" + v + "}}",
               body='{ "status": "' + status_ + '", "latitude": {{' + v + 'Lat}}, "longitude": {{lng}} }',
               base="{{driverServiceUrl}}",
               desc="Development/test fixture on driver-service (port 8082). Not exposed by the gateway; driver registration is out of scope.",
               tests=status(200) + "\n" + field("status", status_))

def create_ride(name="Passenger requests a ride", save="rideId", key="{{run}}-" , extra_tests=""):
    return req(name, "POST", "/rides", body=RIDE_BODY, headers={"Idempotency-Key": key + save},
               tests=f"""{status(201)}
{field('status', 'REQUESTED', 'starts in REQUESTED')}
pm.test('has rideId', () => pm.expect(pm.response.json().rideId).to.be.a('string'));
pm.collectionVariables.set('{save}', pm.response.json().rideId);
{extra_tests}""")

def ride_is(name, st, driver_var=None, ride_var="rideId", max_s=10, extra=""):
    drv = f"b.assignedDriverId === {var(driver_var)}" if driver_var else "b.assignedDriverId === null"
    cond = f"b.status === '{st}' && {drv}"
    return req(name, "GET", "/rides/{{" + ride_var + "}}",
               pre=poll("{{baseUrl}}/rides/{{" + ride_var + "}}", cond, f"the ride is {st}", max_s),
               tests=f"""{status(200)}
{field('status', st)}
pm.test('assignedDriverId', () => pm.expect(pm.response.json().assignedDriverId).to.eql({var(driver_var) if driver_var else 'null'}));
{extra}""")

def driver_is(name, v, st, max_s=10):
    return req(name, "GET", "/drivers/{{" + v + "}}",
               pre=poll("{{baseUrl}}/drivers/{{" + v + "}}", f"b.status === '{st}'", f"the driver is {st}", max_s),
               tests=f"{status(200)}\n{field('status', st)}")

def offer(name, action, v, ride_var="rideId", tests=""):
    return req(name, "POST", "/rides/{{" + ride_var + "}}/offers/{{" + v + "}}/" + action, tests=tests)

def cancel(name="Cleanup: cancel the ride", ride_var="rideId", expect=200):
    return req(name, "POST", "/rides/{{" + ride_var + "}}/cancel", tests=status(expect))

# ---------- scenarios ----------

items = []

items.append(folder("00 · Health & seeded data",
  "Check the stack is up and look at the seeded records (passengers P001–P010, drivers D001–D020).", [
  req("Gateway health", "GET", "/healthz", tests=status(200)),
  req("Seeded driver D001 (AVAILABLE near Ben Thanh)", "GET", "/drivers/D001",
      tests=status(200) + "\npm.test('has a location', () => pm.expect(pm.response.json().latitude).to.be.a('number'));"),
  req("Seeded driver D011 (OFFLINE)", "GET", "/drivers/D011", tests=status(200)),
  req("Unknown driver → 404", "GET", "/drivers/NOPE", tests=status(404) + "\n" + code("DRIVER_NOT_FOUND")),
]))

s1 = [
  req("Setup: driver A ~300 m (AVAILABLE)", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "AVAILABLE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 300), ("driverB", 1500), ("driverC", 3200)]),
      desc="Development/test fixture on driver-service (port 8082). Not exposed by the gateway.",
      tests=status(200)),
  seed("B ~1.5 km", "driverB"),
  seed("C ~3.2 km (outside 2 km radius)", "driverC"),
  create_ride(),
  ride_is("Ride is DRIVER_ASSIGNED to the nearest driver A", "DRIVER_ASSIGNED", "driverA",
          extra="pm.test('B (1.5 km) and C (3.2 km, outside radius) were not chosen', () => pm.expect(pm.response.json().assignedDriverId).to.not.be.oneOf([" + var("driverB") + ", " + var("driverC") + "]));"),
  req("Driver A sees the pending offer", "GET", "/drivers/{{driverA}}/offer",
      tests=f"""{status(200)}
{field('status', 'PENDING')}
pm.test('offer is for this ride', () => pm.expect(pm.response.json().rideId).to.eql({var('rideId')}));
pm.test('expires in ~30 s', () => {{
  const o = pm.response.json();
  const s = (new Date(o.expiresAt) - new Date(o.createdAt)) / 1000;
  pm.expect(s).to.be.within(29, 31);
}});"""),
  driver_is("Driver A is OFFERED", "driverA", "OFFERED"),
  req("Driver C (outside radius) has no offer → 204", "GET", "/drivers/{{driverC}}/offer", tests=status(204)),
  offer("Driver A accepts", "accept", "driverA", tests=status(200) + "\n" + field("status", "ACCEPTED")),
  ride_is("Ride is DRIVER_ACCEPTED", "DRIVER_ACCEPTED", "driverA"),
  driver_is("Driver A is BUSY", "driverA", "BUSY"),
  req("Driver completes the ride", "POST", "/rides/{{rideId}}/complete", tests=status(200) + "\n" + field("status", "COMPLETED")),
  driver_is("Driver A is AVAILABLE again", "driverA", "AVAILABLE"),
  req("History: every status change", "GET", "/rides/{{rideId}}/history",
      tests=f"""{status(200)}
pm.test('REQUESTED → MATCHING → DRIVER_ASSIGNED → DRIVER_ACCEPTED → COMPLETED', () => {{
  const to = pm.response.json().transitions.map(t => t.to);
  pm.expect(to).to.eql(['REQUESTED', 'MATCHING', 'DRIVER_ASSIGNED', 'DRIVER_ACCEPTED', 'COMPLETED']);
}});"""),
  req("Dispatch view: saga, offers, steps", "GET", "/rides/{{rideId}}/dispatch",
      tests=f"""{status(200)}
pm.test('saga COMPLETED', () => pm.expect(pm.response.json().saga.status).to.eql('COMPLETED'));
pm.test('one offer, ACCEPTED by driver A', () => {{
  const o = pm.response.json().offers;
  pm.expect(o.length).to.eql(1);
  pm.expect(o[0].status).to.eql('ACCEPTED');
  pm.expect(o[0].driverId).to.eql({var('driverA')});
}});"""),
]
items.append(folder("01 · Happy path: request → offer → accept → complete",
  "AC-01, AC-02, AC-04, AC-05, AC-06, AC-10. Three drivers at 300 m, 1.5 km and 3.2 km; the nearest within 2 km gets the offer.", s1))

s2 = [
  req("Setup: new area + key", "GET", "/healthz", pre=area([]), tests=status(200)),
  create_ride("Create ride (Idempotency-Key = {{run}}-rideId)"),
  req("Same key + same body → original ride, no new ride", "POST", "/rides", body=RIDE_BODY,
      headers={"Idempotency-Key": "{{run}}-rideId"},
      tests=f"""{status(201)}
pm.test('same rideId', () => pm.expect(pm.response.json().rideId).to.eql({var('rideId')}));
pm.test('Idempotent-Replayed: true', () => pm.expect(pm.response.headers.get('Idempotent-Replayed')).to.eql('true'));"""),
  req("Same key + same body, third time", "POST", "/rides", body=RIDE_BODY,
      headers={"Idempotency-Key": "{{run}}-rideId"},
      tests=f"pm.test('same rideId', () => pm.expect(pm.response.json().rideId).to.eql({var('rideId')}));"),
  req("Same key + different body → 422 IDEMPOTENCY_KEY_REUSED", "POST", "/rides",
      body=RIDE_BODY.replace("P001", "P002"), headers={"Idempotency-Key": "{{run}}-rideId"},
      tests=status(422) + "\n" + code("IDEMPOTENCY_KEY_REUSED")),
  req("No Idempotency-Key → 400", "POST", "/rides", body=RIDE_BODY,
      tests=status(400) + "\n" + code("IDEMPOTENCY_KEY_REQUIRED")),
  req("Created ride can be retrieved", "GET", "/rides/{{rideId}}",
      tests=status(200) + f"\npm.test('same ride', () => pm.expect(pm.response.json().rideId).to.eql({var('rideId')}));"),
  cancel(),
]
items.append(folder("02 · Idempotent ride creation",
  "AC-01, AC-08. Repeating a request with the same Idempotency-Key returns the original ride; reusing the key with another body is rejected.", s2))

def bad(name, body, c, st=422, key=True):
    return req(name, "POST", "/rides", body=body, headers={"Idempotency-Key": "{{$guid}}"} if key else None,
               tests=status(st) + "\n" + code(c))

s3 = [
  bad("Latitude 91 → 422", '{ "passengerId": "P001", "pickup": { "latitude": 91, "longitude": 106.7 }, "destination": { "latitude": 10.8, "longitude": 106.6 } }', "INVALID_COORDINATES"),
  bad("Latitude -90.5 → 422", '{ "passengerId": "P001", "pickup": { "latitude": 10.7, "longitude": 106.7 }, "destination": { "latitude": -90.5, "longitude": 106.6 } }', "INVALID_COORDINATES"),
  bad("Longitude 180.1 → 422", '{ "passengerId": "P001", "pickup": { "latitude": 10.7, "longitude": 180.1 }, "destination": { "latitude": 10.8, "longitude": 106.6 } }', "INVALID_COORDINATES"),
  bad("Missing longitude → 422", '{ "passengerId": "P001", "pickup": { "latitude": 10.7 }, "destination": { "latitude": 10.8, "longitude": 106.6 } }', "INVALID_COORDINATES"),
  bad("Missing destination → 422", '{ "passengerId": "P001", "pickup": { "latitude": 10.7, "longitude": 106.7 } }', "INVALID_COORDINATES"),
  bad("Missing passengerId → 422", '{ "pickup": { "latitude": 10.7, "longitude": 106.7 }, "destination": { "latitude": 10.8, "longitude": 106.6 } }', "PASSENGER_REQUIRED"),
  bad("Unknown passenger P999 → 422", '{ "passengerId": "P999", "pickup": { "latitude": 10.7, "longitude": 106.7 }, "destination": { "latitude": 10.8, "longitude": 106.6 } }', "UNKNOWN_PASSENGER"),
  bad("Malformed JSON → 400", '{ "passengerId": ', "INVALID_JSON", st=400),
  req("Driver location latitude 95 → 422", "PUT", "/drivers/D001/location", body='{ "latitude": 95, "longitude": 106.7 }',
      tests=status(422) + "\n" + code("INVALID_COORDINATES")),
  req("Driver availability BUSY (not allowed via API) → 422", "PUT", "/drivers/D011/availability", body='{ "status": "BUSY" }',
      tests=status(422) + "\n" + code("INVALID_STATUS")),
  req("Unknown ride → 404", "GET", "/rides/00000000-0000-7000-8000-000000000000", tests=status(404) + "\n" + code("RIDE_NOT_FOUND")),
  req("/internal is not exposed by the gateway → 404", "GET", "/internal/drivers", tests=status(404)),
]
items.append(folder("03 · Validation", "AC-01, AC-03. Invalid input is rejected before any matching.", s3))

s4 = [
  req("Setup: driver A ~200 m", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "AVAILABLE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 200), ("driverB", 900)]), tests=status(200)),
  seed("B ~900 m", "driverB"),
  create_ride(),
  ride_is("Offer goes to A (nearest)", "DRIVER_ASSIGNED", "driverA"),
  offer("A rejects", "reject", "driverA", tests=status(200) + "\n" + field("status", "REJECTED")),
  offer("A rejects again → same result (idempotent)", "reject", "driverA",
        tests=status(200) + "\npm.test('Idempotent-Replayed: true', () => pm.expect(pm.response.headers.get('Idempotent-Replayed')).to.eql('true'));"),
  ride_is("Rematched: ride now DRIVER_ASSIGNED to B", "DRIVER_ASSIGNED", "driverB"),
  driver_is("A is AVAILABLE again", "driverA", "AVAILABLE"),
  offer("A cannot accept any more → 409 OFFER_NOT_CURRENT", "accept", "driverA", tests=status(409) + "\n" + code("OFFER_NOT_CURRENT")),
  offer("B rejects too", "reject", "driverB", tests=status(200)),
  ride_is("No eligible driver left: ride stays MATCHING, no driver (A is available but excluded)", "MATCHING", None),
  req("Dispatch view: A and B excluded for this ride", "GET", "/rides/{{rideId}}/dispatch",
      tests=f"""{status(200)}
pm.test('both excluded', () => {{
  const ex = pm.response.json().exclusions;
  pm.expect(ex).to.have.property({var('driverA')});
  pm.expect(ex).to.have.property({var('driverB')});
}});
pm.test('saga waiting for a driver', () => pm.expect(pm.response.json().saga.status).to.be.oneOf(['WAITING_FOR_DRIVER', 'MATCHING']));"""),
  cancel(),
]
items.append(folder("04 · Reject → rematch → exclusion",
  "AC-05, AC-06, AC-07. A rejected driver returns to AVAILABLE, is never offered this ride again, and the next driver is offered. With nobody left the ride stays MATCHING with no assignedDriverId.", s4))

s5 = [
  req("Setup: driver A ~100 m", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "AVAILABLE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 100), ("driverB", 50000)]), tests=status(200)),
  create_ride(),
  ride_is("Offer goes to A", "DRIVER_ASSIGNED", "driverA"),
  offer("A accepts", "accept", "driverA",
        tests=status(200) + "\n" + field("status", "ACCEPTED") + "\npm.collectionVariables.set('offerId', pm.response.json().offerId);"),
  offer("A accepts again → 200, same offer, nothing changes", "accept", "driverA",
        tests=f"""{status(200)}
pm.test('same offer', () => pm.expect(pm.response.json().offerId).to.eql({var('offerId')}));
pm.test('Idempotent-Replayed: true', () => pm.expect(pm.response.headers.get('Idempotent-Replayed')).to.eql('true'));"""),
  offer("A accepts a third time", "accept", "driverA", tests=status(200)),
  offer("Another driver cannot accept → 404 OFFER_NOT_FOUND", "accept", "driverB", tests=status(404) + "\n" + code("OFFER_NOT_FOUND")),
  ride_is("Ride is DRIVER_ACCEPTED (valid state)", "DRIVER_ACCEPTED", "driverA"),
  driver_is("A is BUSY (only once)", "driverA", "BUSY"),
  req("History has exactly one DRIVER_ACCEPTED", "GET", "/rides/{{rideId}}/history",
      tests=f"""{status(200)}
pm.test('one acceptance', () => pm.expect(pm.response.json().transitions.filter(t => t.to === 'DRIVER_ACCEPTED').length).to.eql(1));"""),
  req("Cleanup: complete", "POST", "/rides/{{rideId}}/complete", tests=status(200)),
]
items.append(folder("05 · Duplicate driver acceptance",
  "AC-05, AC-08. Repeating DriverAccepted must not produce an invalid state.", s5))

race_pre = """// Wait until both rides are settled, then record which one holds the driver.
const base = pm.variables.replaceIn('{{baseUrl}}/rides/');
const ids = [pm.collectionVariables.get('ride1'), pm.collectionVariables.get('ride2')];
(function poll(n) {
  pm.sendRequest(base + ids[0], (e1, r1) => {
    pm.sendRequest(base + ids[1], (e2, r2) => {
      const a = r1.json(), b = r2.json();
      const settled = [a.status, b.status].sort().join() === 'DRIVER_ASSIGNED,MATCHING';
      if (settled) {
        const winner = a.status === 'DRIVER_ASSIGNED' ? ids[0] : ids[1];
        pm.collectionVariables.set('winner', winner);
        pm.collectionVariables.set('loser', winner === ids[0] ? ids[1] : ids[0]);
        return;
      }
      if (n > 0) setTimeout(() => poll(n - 1), 250);
    });
  });
})(40);"""
s6 = [
  req("Setup: the only driver A ~150 m", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "AVAILABLE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 150)]), tests=status(200)),
  create_ride("Ride 1 requested", save="ride1"),
  create_ride("Ride 2 requested right after (both sagas race for A)", save="ride2"),
  req("Driver A holds exactly one offer", "GET", "/drivers/{{driverA}}/offer", pre=race_pre,
      tests=f"""{status(200)}
pm.test('offer belongs to the winning ride', () => pm.expect(pm.response.json().rideId).to.eql({var('winner')}));"""),
  ride_is("Winner ride is DRIVER_ASSIGNED to A", "DRIVER_ASSIGNED", "driverA", ride_var="winner"),
  ride_is("Loser ride stays MATCHING with no driver", "MATCHING", None, ride_var="loser"),
  driver_is("A is OFFERED (once)", "driverA", "OFFERED"),
  cancel("Winner's passenger cancels", ride_var="winner"),
  ride_is("A is released and now offered to the waiting ride", "DRIVER_ASSIGNED", "driverA", ride_var="loser", max_s=15),
  cancel("Cleanup: cancel the other ride", ride_var="loser"),
]
items.append(folder("06 · Two rides, one driver (concurrency)",
  "AC-09. Two rides compete for the only nearby driver: exactly one gets the offer, the other stays MATCHING. "
  "Postman sends requests one after another; true parallel races are covered by the Go tests (50 rounds against Postgres) and `make demo` step 9.", s6))

s7 = [
  req("Setup: driver A ~100 m", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "AVAILABLE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 100)]), tests=status(200)),
  create_ride(),
  ride_is("Offer goes to A", "DRIVER_ASSIGNED", "driverA"),
  req("OFFERED driver cannot go OFFLINE → 409", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "OFFLINE" }',
      tests=status(409) + "\n" + code("INVALID_TRANSITION")),
  req("Passenger cancels (DRIVER_ASSIGNED → CANCELLED)", "POST", "/rides/{{rideId}}/cancel",
      tests=status(200) + "\n" + field("status", "CANCELLED")),
  req("Cancel again → 200, no-op", "POST", "/rides/{{rideId}}/cancel", tests=status(200) + "\n" + field("status", "CANCELLED")),
  driver_is("A is AVAILABLE again (compensation)", "driverA", "AVAILABLE"),
  offer("A accepts after cancel → 409", "accept", "driverA", tests=status(409) + "\n" + code("OFFER_NOT_CURRENT")),
  req("Completing a CANCELLED ride → 409", "POST", "/rides/{{rideId}}/complete", tests=status(409) + "\n" + code("INVALID_TRANSITION")),
  req("Dispatch view: offer CANCELLED, saga CANCELLED", "GET", "/rides/{{rideId}}/dispatch",
      tests=f"""{status(200)}
pm.test('saga CANCELLED', () => pm.expect(pm.response.json().saga.status).to.eql('CANCELLED'));
pm.test('offer CANCELLED', () => pm.expect(pm.response.json().offers[0].status).to.eql('CANCELLED'));"""),
]
items.append(folder("07 · Cancel releases the driver",
  "AC-02, AC-05, AC-06. Cancelling a pending offer returns the driver from OFFERED to AVAILABLE.", s7))

s7b = [
  req("Setup: driver A ~100 m", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "AVAILABLE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 100)]), tests=status(200)),
  create_ride(),
  ride_is("Offer goes to A", "DRIVER_ASSIGNED", "driverA"),
  offer("A accepts", "accept", "driverA", tests=status(200)),
  driver_is("A is BUSY", "driverA", "BUSY"),
  req("BUSY driver cannot go OFFLINE → 409", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "OFFLINE" }',
      tests=status(409)),
  ride_is("Ride is DRIVER_ACCEPTED", "DRIVER_ACCEPTED", "driverA"),
  req("Cancel an accepted ride (DRIVER_ACCEPTED → CANCELLED)", "POST", "/rides/{{rideId}}/cancel",
      tests=status(200) + "\n" + field("status", "CANCELLED")),
  driver_is("A goes BUSY → AVAILABLE", "driverA", "AVAILABLE"),
]
items.append(folder("08 · Cancel after acceptance",
  "AC-02, AC-05, AC-06. Cancelling an accepted ride returns the driver from BUSY to AVAILABLE.", s7b))

s8 = [
  req("Setup: new area with no drivers", "GET", "/healthz", pre=area([]), tests=status(200)),
  create_ride(),
  ride_is("No driver nearby: ride stays MATCHING", "MATCHING", None),
  req("Complete while MATCHING → 409", "POST", "/rides/{{rideId}}/complete", tests=status(409) + "\n" + code("INVALID_TRANSITION")),
  req("Cancel while MATCHING → 200", "POST", "/rides/{{rideId}}/cancel", tests=status(200) + "\n" + field("status", "CANCELLED")),
  req("Complete a CANCELLED (terminal) ride → 409", "POST", "/rides/{{rideId}}/complete", tests=status(409)),
  req("History: REQUESTED → MATCHING → CANCELLED", "GET", "/rides/{{rideId}}/history",
      tests=f"""{status(200)}
pm.test('transitions', () => pm.expect(pm.response.json().transitions.map(t => t.to)).to.eql(['REQUESTED', 'MATCHING', 'CANCELLED']));"""),
]
items.append(folder("09 · Ride state machine: invalid transitions",
  "AC-06. Valid transitions succeed, invalid ones return 409 INVALID_TRANSITION, terminal states stay terminal, duplicates are no-ops.", s8))

s9 = [
  req("Setup: driver A OFFLINE ~5 km away", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "OFFLINE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 5000), ("driverB", 50)]), tests=status(200)),
  req("A: OFFLINE → AVAILABLE", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "AVAILABLE" }',
      tests=status(200) + "\n" + field("status", "AVAILABLE")),
  req("A: AVAILABLE again → 200, no-op", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "AVAILABLE" }',
      tests=status(200) + "\n" + field("status", "AVAILABLE")),
  req("A: AVAILABLE → OFFLINE", "PUT", "/drivers/{{driverA}}/availability", body='{ "status": "OFFLINE" }',
      tests=status(200) + "\n" + field("status", "OFFLINE")),
  create_ride("Ride requested (A is offline and 5 km away)"),
  ride_is("Nobody eligible: MATCHING", "MATCHING", None),
  req("A moves next to the pickup (latest location)", "PUT", "/drivers/{{driverA}}/location",
      body='{ "latitude": {{driverBLat}}, "longitude": {{lng}} }',
      tests=f"""{status(200)}
pm.test('latest location stored', () => pm.expect(pm.response.json().latitude).to.be.closeTo(Number({var('driverBLat')}), 1e-6));"""),
  req("A goes AVAILABLE → the waiting ride is matched using the latest location", "PUT", "/drivers/{{driverA}}/availability",
      body='{ "status": "AVAILABLE" }', tests=status(200)),
  ride_is("Ride is DRIVER_ASSIGNED to A", "DRIVER_ASSIGNED", "driverA", max_s=15),
  req("GET driver A shows the latest location", "GET", "/drivers/{{driverA}}",
      tests=f"""{status(200)}
pm.test('latest latitude', () => pm.expect(pm.response.json().latitude).to.be.closeTo(Number({var('driverBLat')}), 1e-6));"""),
  cancel(),
]
items.append(folder("10 · Driver availability & latest location",
  "AC-02, AC-03. OFFLINE ⇄ AVAILABLE through the API, latest location stored and used by matching. A ride waiting in MATCHING is retried as soon as a driver becomes available.", s9))

s10 = [
  req("Setup: driver A ~100 m", "PUT", "/internal/drivers/{{driverA}}",
      body='{ "status": "AVAILABLE", "latitude": {{driverALat}}, "longitude": {{lng}} }', base="{{driverServiceUrl}}",
      pre=area([("driverA", 100), ("driverB", 600)]), tests=status(200)),
  seed("B ~600 m", "driverB"),
  create_ride(),
  ride_is("Offer goes to A", "DRIVER_ASSIGNED", "driverA"),
  ride_is("A does not answer… after the timeout the ride is offered to B (≈ 31 s)", "DRIVER_ASSIGNED", "driverB", max_s=45),
  driver_is("A is AVAILABLE again", "driverA", "AVAILABLE"),
  offer("A accepts too late → 409", "accept", "driverA", tests=status(409)),
  req("Dispatch view: A's offer EXPIRED, A excluded", "GET", "/rides/{{rideId}}/dispatch",
      tests=f"""{status(200)}
pm.test('first offer EXPIRED', () => pm.expect(pm.response.json().offers[0].status).to.eql('EXPIRED'));
pm.test('A excluded (EXPIRED)', () => pm.expect(pm.response.json().exclusions[{var('driverA')}]).to.eql('EXPIRED'));"""),
  cancel(),
]
items.append(folder("11 · Offer timeout (slow, ~35 s)",
  "AC-05, AC-06. No answer within OFFER_TIMEOUT (default 30 s) is treated as a rejection. Tip: start the stack with `OFFER_TIMEOUT=5s make up` to make this fast.", s10))

collection = {
    "info": {
        "name": "Ride-Hailing Dispatch (kvp)",
        "description": (
            "Scenarios for Coding Challenge #3. Start the stack first (`make up` in coding-challenge-3/kvp).\n\n"
            "* Run the whole collection with the **Runner** (keep the order), or one folder at a time.\n"
            "* Every folder is self-contained: its first request picks a random spot on the map and creates its own drivers "
            "through the driver-service fixture API (`{{driverServiceUrl}}/internal/drivers/...`), so folders never interfere.\n"
            "* Dispatch is asynchronous. Requests that check the result first wait (pre-request script, max 10–45 s) until "
            "the expected state is reached.\n"
            "* Variables: `baseUrl` (gateway, default http://localhost:8080) and `driverServiceUrl` (http://localhost:8082)."),
        "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
    },
    "variable": [
        {"key": "baseUrl", "value": "http://localhost:8080"},
        {"key": "driverServiceUrl", "value": "http://localhost:8082"},
    ],
    "item": items,
}
json.dump(collection, open(OUT, "w"), indent=2, ensure_ascii=False)
n = sum(len(f["item"]) for f in items)
print(f"{len(items)} folders, {n} requests -> {OUT}")
