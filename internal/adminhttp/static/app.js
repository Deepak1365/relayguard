// RelayGuard admin UI: a thin client over the JSON API in the same origin.
//
// Rule for this file: every piece of text that did not come from this file
// (payloads, actor, reason, last_error, explanations, JSON images) is put on
// the page with textContent or createElement, never parsed as markup. The CSP
// (default-src 'self') is the second line of defence, not the first.
(function () {
  "use strict";

  var app = document.getElementById("app");
  // Bumped on every navigation; a slow response for an old view must not
  // overwrite the view the operator has since moved to.
  var viewToken = 0;

  // ---- small DOM helpers (text only) ----

  // Attribute names el() may set. Anything else throws, so a future el() call setting an on* handler
  // or inline-style attribute cannot slip in unnoticed (a source test checks this list).
  var ALLOWED_ATTRS = ["href", "type", "id", "for", "role", "maxlength", "autocomplete", "required"];

  // el("a", {href: "#/x", class: "c", text: "hi"}, [children])
  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        if (k === "text") node.textContent = attrs[k];
        else if (k === "class") node.className = attrs[k];
        else if (ALLOWED_ATTRS.indexOf(k) >= 0) node.setAttribute(k, attrs[k]);
        else throw new Error("el: attribute not allowed: " + k);
      });
    }
    (children || []).forEach(function (c) {
      if (c !== null && c !== undefined) node.appendChild(c);
    });
    return node;
  }

  function text(s) { return document.createTextNode(String(s)); }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function pretty(v) { return JSON.stringify(v === undefined ? null : v, null, 2); }

  function jsonBlock(v) { return el("pre", { text: pretty(v) }); }

  function eventLink(id, label) {
    return el("a", { href: "#/event/" + encodeURIComponent(id), text: label || ("#" + id) });
  }

  function customerLink(id) {
    if (!id) return el("span", { class: "muted", text: "-" });
    return el("a", { href: "#/customer/" + encodeURIComponent(id), text: id });
  }

  function statusCell(s) {
    return el("span", { class: "status-" + String(s).replace(/[^a-z]/g, ""), text: s });
  }

  function isoFromUnix(n) {
    if (n === null || n === undefined) return "-";
    var d = new Date(n * 1000);
    return isNaN(d.getTime()) ? String(n) : n + " (" + d.toISOString() + ")";
  }

  function notice(kind, nodes) {
    return el("div", { class: "notice " + kind, role: kind === "error" ? "alert" : "status" }, nodes);
  }

  function p(s) { return el("p", { text: s }); }

  // ---- API ----

  // api never throws. It returns {status, data}; data is the parsed JSON body
  // or null when the body is not JSON (so raw response text is never shown).
  // status 0 means the request itself failed.
  function api(method, path, body) {
    var opts = { method: method, headers: {} };
    if (method === "POST") {
      // The server requires application/json on every POST.
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body || {});
    }
    return fetch(path, opts).then(function (resp) {
      return resp.text().then(function (t) {
        var data = null;
        try { data = JSON.parse(t); } catch (e) { data = null; }
        return { status: resp.status, data: data };
      });
    }).catch(function () {
      return { status: 0, data: null };
    });
  }

  // errorNotice renders a failed API response per the plan's error table.
  function errorNotice(res) {
    var d = res.data;
    var nodes = [];
    if (res.status === 0) {
      return notice("error", [p("Could not reach the server. Is `admin serve` still running?")]);
    }
    if (!d || typeof d.message !== "string") {
      return notice("error", [p("Unexpected response (HTTP " + res.status + ").")]);
    }
    nodes.push(el("p", {}, [el("strong", { text: "Error (HTTP " + res.status + (d.error ? ", " + d.error : "") + "): " }), text(d.message)]));

    var r = d.result;
    if (res.status === 503) {
      nodes.push(p("The rows are locked by something else. Try again in a moment."));
    } else if (d.error === "changed") {
      nodes.push(p("The event changed during the preview. Run the preview again."));
    } else if (d.error === "not_previewable" || d.error === "undone") {
      nodes.push(p("See the change log below for what happened to this event."));
    }
    if (r && (d.error === "not_replayable" || d.error === "not_undoable") && r.action_id) {
      nodes.push(el("p", {}, [text("Refusal recorded as audit action "),
        el("a", { href: "#/actions", text: "#" + r.action_id }), text(".")]));
    }
    if (d.error === "conflict" && r && Array.isArray(r.conflicts)) {
      if (r.action_id) {
        nodes.push(el("p", {}, [text("Refusal recorded as audit action "),
          el("a", { href: "#/actions", text: "#" + r.action_id }), text(".")]));
      }
      var ul = el("ul");
      r.conflicts.forEach(function (c) {
        var li = el("li");
        li.appendChild(el("div", {}, [el("strong", { text: c.kind }),
          text(" on " + c.table + " row " + c.row_id)]));
        li.appendChild(el("div", { text: c.explanation }));
        if (c.inbox_id) {
          li.appendChild(el("div", {}, [text("Related event: "), eventLink(c.inbox_id,
            "#" + c.inbox_id + (c.event_type ? " " + c.event_type : ""))]));
        }
        ul.appendChild(li);
      });
      nodes.push(ul);
    }
    return notice("error", nodes);
  }

  function changesView(changes) {
    var box = el("div");
    if (!changes || changes.length === 0) {
      box.appendChild(el("p", { class: "muted", text: "No changes." }));
      return box;
    }
    changes.forEach(function (c) {
      var head = c.table + " #" + c.row_id + " " + c.op;
      if (c.seq !== undefined) head = "seq " + c.seq + ": " + head;
      var meta = [];
      if (c.generation !== undefined) meta.push("generation " + c.generation);
      if (c.source) meta.push("source " + c.source);
      if (c.admin_action_id) meta.push("action #" + c.admin_action_id);
      if (c.created_at) meta.push(c.created_at);
      box.appendChild(el("div", { class: "change" }, [
        el("strong", { text: head }),
        meta.length ? el("div", { class: "muted", text: meta.join(" | ") }) : null,
        el("div", { class: "cols" }, [
          el("div", {}, [el("div", { class: "muted", text: "before" }), jsonBlock(c.before)]),
          el("div", {}, [el("div", { class: "muted", text: "after" }), jsonBlock(c.after)])
        ])
      ]));
    });
    return box;
  }

  // ---- list views ----

  function th(labels) {
    return el("thead", {}, [el("tr", {}, labels.map(function (l) { return el("th", { text: l }); }))]);
  }

  function eventRow(e) {
    return el("tr", {}, [
      el("td", {}, [eventLink(e.id)]),
      el("td", { text: e.event_type }),
      el("td", {}, [statusCell(e.status)]),
      el("td", { text: e.attempts }),
      el("td", { class: "wrap", text: e.last_error }),
      el("td", { text: e.received_at }),
      el("td", { text: isoFromUnix(e.event_created) }),
      el("td", {}, [customerLink(e.customer_id)])
    ]);
  }

  // pagedEvents shows a table and an "Older" button that appends the next
  // keyset page (before=<id>), so rows never shift under the operator.
  function pagedEvents(url, token, heading, intro) {
    app.appendChild(el("h2", { text: heading }));
    if (intro) app.appendChild(el("p", { class: "muted", text: intro }));
    var tbody = el("tbody");
    var table = el("table", {}, [th(["Inbox id", "Type", "Status", "Attempts", "Last error", "Received", "Event created", "Customer"]), tbody]);
    var more = el("button", { type: "button", text: "Older" });
    var empty = el("p", { class: "muted", text: "No events." });
    var status = el("div");
    app.appendChild(el("div", { class: "scroll" }, [table]));
    app.appendChild(empty);
    app.appendChild(status);
    app.appendChild(more);
    empty.hidden = true;
    more.hidden = true;
    var before = 0;
    var shown = 0;

    function load() {
      more.disabled = true;
      clear(status);
      var u = url + (before ? (url.indexOf("?") >= 0 ? "&" : "?") + "before=" + before : "");
      api("GET", u).then(function (res) {
        if (token !== viewToken) return;
        if (res.status !== 200 || !res.data || !Array.isArray(res.data.events)) {
          status.appendChild(errorNotice(res));
          more.disabled = false;
          more.hidden = false;
          return;
        }
        res.data.events.forEach(function (e) { tbody.appendChild(eventRow(e)); });
        shown += res.data.events.length;
        empty.hidden = shown !== 0;
        before = res.data.next_before;
        more.hidden = !before;
        more.disabled = false;
      });
    }
    more.addEventListener("click", load);
    load();
  }

  function viewEvents(token, params) {
    var status = params.get("status") || "";
    var url = "/api/events" + (status ? "?status=" + encodeURIComponent(status) : "");
    var intro = status === "failed" ? "Still retrying: pending with a last error." : "";
    pagedEvents(url, token, status ? "Events: " + status : "All events", intro);
  }

  function viewUnattributed(token) {
    pagedEvents("/api/events/unattributed", token, "Unattributed events",
      "Events no customer could be derived from (for example an undecodable payload).");
  }

  function viewCustomer(token, id) {
    pagedEvents("/api/customers/" + encodeURIComponent(id) + "/timeline", token, "Timeline: " + id,
      "Newest arrival first (by inbox id). 'Event created' is the provider's timestamp from the payload, so out-of-order delivery shows as created times that go up and down.");
  }

  function viewActions(token) {
    app.appendChild(el("h2", { text: "Audit log" }));
    var tbody = el("tbody");
    var more = el("button", { type: "button", text: "Older" });
    var empty = el("p", { class: "muted", text: "No admin actions yet." });
    var status = el("div");
    app.appendChild(el("div", { class: "scroll" }, [el("table", {}, [
      th(["Action", "Event", "Kind", "Outcome", "Actor", "Reason", "When", "Detail"]), tbody])]));
    app.appendChild(empty);
    app.appendChild(status);
    app.appendChild(more);
    empty.hidden = true;
    more.hidden = true;
    var before = 0;
    var shown = 0;

    function load() {
      more.disabled = true;
      clear(status);
      api("GET", "/api/actions" + (before ? "?before=" + before : "")).then(function (res) {
        if (token !== viewToken) return;
        if (res.status !== 200 || !res.data || !Array.isArray(res.data.actions)) {
          status.appendChild(errorNotice(res));
          more.disabled = false;
          more.hidden = false;
          return;
        }
        res.data.actions.forEach(function (a) { tbody.appendChild(actionRow(a)); });
        shown += res.data.actions.length;
        empty.hidden = shown !== 0;
        before = res.data.next_before;
        more.hidden = !before;
        more.disabled = false;
      });
    }
    more.addEventListener("click", load);
    load();
  }

  function actionRow(a) {
    var detail = el("details", {}, [el("summary", { text: "detail" }), jsonBlock(a.detail)]);
    return el("tr", {}, [
      el("td", { text: "#" + a.id }),
      el("td", {}, [eventLink(a.inbox_id)]),
      el("td", { text: a.action }),
      el("td", { text: a.outcome }),
      el("td", { class: "wrap", text: a.actor }),
      el("td", { class: "wrap", text: a.reason }),
      el("td", { text: a.created_at }),
      el("td", {}, [detail])
    ]);
  }

  // ---- event detail (the log) and actions ----

  function previewNotice(d) {
    var nodes = [el("p", {}, [el("strong", { text: "Preview (nothing was changed): " }),
      text(d.outcome + (d.reason ? " - " + d.reason : "") + " (status " + d.status + ", generation " + d.generation + ")")])];
    nodes.push(changesView(d.changes));
    return notice("ok", nodes);
  }

  function replayNotice(d) {
    return notice("ok", [el("p", {}, [el("strong", { text: "Replay: " }),
      text(d.outcome + (d.reason ? " - " + d.reason : "") + ". Audit action #" + d.action_id +
        "; was " + d.from_status + " after " + d.from_attempts + " attempts; now generation " + d.generation +
        ". The worker will pick it up.")])]);
  }

  function undoNotice(d) {
    var nodes = [el("p", {}, [el("strong", { text: "Undo: " }),
      // d.status is the status BEFORE the undo (admin.UndoResult.Status); a
      // successful undo leaves the event undone, as the CLI's FormatUndo says.
      text(d.outcome + (d.reason ? " - " + d.reason : "") + ". Audit action #" + d.action_id +
        "; was " + d.status + ", generation " + d.generation + "; event status now undone.")])];
    if (d.warnings && d.warnings.length) {
      var ul = el("ul");
      d.warnings.forEach(function (w) { ul.appendChild(el("li", { text: w })); });
      nodes.push(ul);
    }
    nodes.push(changesView(d.changes));
    return notice("ok", nodes);
  }

  function viewEvent(token, id, pendingNotice) {
    // First load (not the re-read after an action): say so while the GET is out.
    if (!pendingNotice) {
      clear(app);
      app.appendChild(p("Loading..."));
    }
    api("GET", "/api/events/" + encodeURIComponent(id)).then(function (res) {
      if (token !== viewToken) return;
      clear(app);
      app.appendChild(el("h2", { text: "Event #" + id }));
      if (pendingNotice) app.appendChild(pendingNotice);
      if (res.status !== 200 || !res.data) {
        app.appendChild(errorNotice(res));
        return;
      }
      renderEvent(token, id, res.data);
    });
  }

  function kv(label, value) {
    return el("tr", {}, [el("th", { text: label }), el("td", { class: "wrap" }, [typeof value === "string" ? text(value) : value])]);
  }

  function renderEvent(token, id, d) {
    app.appendChild(el("div", { class: "scroll" }, [el("table", {}, [el("tbody", {}, [
      kv("Provider event id", d.event_id),
      kv("Type", d.event_type),
      kv("Status", statusCell(d.status)),
      kv("Attempts", String(d.attempts)),
      kv("Last error", d.last_error || "-")
    ])])]));

    app.appendChild(actionForm(token, id));

    app.appendChild(el("h3", { text: "Change log (" + d.changes.length + ")" }));
    app.appendChild(changesView(d.changes));

    app.appendChild(el("h3", { text: "Admin actions on this event (" + d.actions.length + ")" }));
    if (d.actions.length === 0) {
      app.appendChild(el("p", { class: "muted", text: "None." }));
    } else {
      var tbody = el("tbody");
      d.actions.forEach(function (a) { tbody.appendChild(actionRow(a)); });
      app.appendChild(el("div", { class: "scroll" }, [el("table", {}, [
        th(["Action", "Event", "Kind", "Outcome", "Actor", "Reason", "When", "Detail"]), tbody])]));
    }
  }

  // actionForm: actor and reason are required every time. They are never
  // pre-filled and never stored (no browser storage, no cookies): the audit row
  // should name whoever is clicking now.
  function actionForm(token, id) {
    var actor = el("input", { id: "actor", type: "text", maxlength: "200", autocomplete: "off", required: "required" });
    var reason = el("textarea", { id: "reason", maxlength: "1000", autocomplete: "off" });
    var msg = el("div");
    var bPrev = el("button", { type: "button", text: "Preview apply (writes nothing)" });
    var bReplay = el("button", { type: "button", text: "Replay" });
    var bUndo = el("button", { type: "button", text: "Undo" });
    var all = [bPrev, bReplay, bUndo];

    function busy(on) { all.forEach(function (b) { b.disabled = on; }); }

    function run(kind) {
      clear(msg);
      var body = {};
      if (kind !== "preview") {
        body = { actor: actor.value.trim(), reason: reason.value.trim() };
        if (!body.actor || !body.reason) {
          msg.appendChild(notice("warn", [p("Actor and reason are both required.")]));
          return;
        }
      }
      busy(true);
      api("POST", "/api/events/" + encodeURIComponent(id) + "/" + kind, body).then(function (res) {
        if (token !== viewToken) return;
        var n;
        if (res.status === 200 && res.data) {
          n = kind === "preview" ? previewNotice(res.data) : kind === "replay" ? replayNotice(res.data) : undoNotice(res.data);
        } else {
          n = errorNotice(res);
        }
        // Success or failure, re-read the log: the screen must show what the
        // database says now, not what this page assumed. This also resets the form.
        viewEvent(token, id, n);
      });
    }

    bPrev.addEventListener("click", function () { run("preview"); });
    bReplay.addEventListener("click", function () { run("replay"); });
    bUndo.addEventListener("click", function () { run("undo"); });

    return el("section", {}, [
      el("h3", { text: "Actions" }),
      el("p", { class: "muted", text: "Preview shows what applying this event would change (not for done events). Replay applies to dead or stale events; Undo reverts the changes of a done event. Both are audited and may be refused." }),
      el("div", { class: "field" }, [el("label", { for: "actor", text: "Actor (who is doing this)" }), actor]),
      el("div", { class: "field" }, [el("label", { for: "reason", text: "Reason" }), reason]),
      el("div", { class: "buttons" }, all),
      msg
    ]);
  }

  // ---- routing ----

  function route() {
    viewToken++;
    var token = viewToken;
    clear(app);
    var hash = location.hash.replace(/^#/, "") || "/events?status=dead";
    var q = hash.indexOf("?");
    var path = q >= 0 ? hash.slice(0, q) : hash;
    var params = new URLSearchParams(q >= 0 ? hash.slice(q + 1) : "");
    var parts = path.split("/").filter(function (s) { return s !== ""; });
    var arg = "";
    try { arg = parts[1] ? decodeURIComponent(parts[1]) : ""; } catch (e) { arg = ""; }

    if (parts[0] === "events") viewEvents(token, params);
    else if (parts[0] === "unattributed") viewUnattributed(token);
    else if (parts[0] === "actions") viewActions(token);
    else if (parts[0] === "customer" && arg && arg !== "." && arg !== "..") viewCustomer(token, arg);
    else if (parts[0] === "event" && /^[1-9][0-9]*$/.test(arg)) viewEvent(token, arg, null);
    else app.appendChild(notice("warn", [p("Unknown page.")]));
  }

  document.getElementById("customer-form").addEventListener("submit", function (ev) {
    ev.preventDefault();
    var input = document.getElementById("customer-id");
    var v = input.value.trim();
    if (v) location.hash = "#/customer/" + encodeURIComponent(v);
  });

  // Clicking the link for the page you are already on fires no hashchange;
  // re-render so the operator can refresh the list.
  document.getElementById("nav").addEventListener("click", function (ev) {
    var a = ev.target.closest ? ev.target.closest("a") : null;
    if (a && a.getAttribute("href") === location.hash) route();
  });

  window.addEventListener("hashchange", route);
  route();
})();
