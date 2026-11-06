"use strict";

// Cercle interface. Any text coming from a peer (names, paths, messages) is
// inserted as text, never as HTML.

// ---------- Helpers ----------

function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "checked" || k === "disabled" || k === "value") el[k] = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (method !== "GET") opts.headers["X-Cercle-UI"] = "1";
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const r = await fetch("/ui/api" + path, opts);
  if (r.status === 204 || r.status === 202) return null;
  const data = await r.json().catch(() => ({}));
  if (r.status === 401 && data.login && path !== "/login") {
    showLogin();
    const e = new Error("Login required.");
    e.login = true;
    throw e;
  }
  if (!r.ok) {
    const e = new Error(data.error || "Error " + r.status);
    e.status = r.status;
    e.fields = data.fields;
    throw e;
  }
  return data;
}

const nf1 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 });
const nf0 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });

function fmtSize(n) {
  n = Number(n) || 0;
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (n < 10 && i > 0 ? nf1 : nf0).format(n) + " " + units[i];
}
const fmtRate = (bps) => fmtSize(bps) + "/s";
const plural = (n, one, many) => nf0.format(n) + " " + (n > 1 ? many : one);

function toDate(v) {
  if (!v) return null;
  const d = typeof v === "number" ? new Date(v / 1e6) : new Date(v);
  return isNaN(d) || d.getFullYear() < 2000 ? null : d;
}
function fmtDate(v) {
  const d = toDate(v);
  return d ? d.toLocaleString("en-GB", { dateStyle: "short", timeStyle: "short" }) : "";
}
function fmtAgo(v) {
  const d = toDate(v);
  if (!d) return "never";
  const s = Math.round((Date.now() - d) / 1000);
  if (s < 0) return "in " + fmtDuration(-s);
  if (s < 45) return "just now";
  return fmtDuration(s) + " ago";
}
function fmtDuration(s) {
  s = Math.max(0, Math.round(s));
  if (s < 60) return s + " s";
  if (s < 3600) return Math.round(s / 60) + " min";
  if (s < 86400) {
    const hh = Math.floor(s / 3600), mm = Math.round((s % 3600) / 60);
    return hh + " h " + String(mm).padStart(2, "0");
  }
  return Math.round(s / 86400) + " d";
}

function toast(msg, bad) {
  const t = h("div", { class: "toast" + (bad ? " bad" : "") }, msg);
  document.getElementById("toasts").append(t);
  setTimeout(() => t.remove(), bad ? 7000 : 4000);
}

async function guard(btn, fn) {
  if (btn) btn.disabled = true;
  try {
    return await fn();
  } catch (e) {
    toast(e.message, true);
  } finally {
    if (btn) btn.disabled = false;
  }
}

function copyButton(getText) {
  return h("button", {
    onclick: async (ev) => {
      try {
        await navigator.clipboard.writeText(getText());
        ev.target.textContent = "Copied";
        setTimeout(() => (ev.target.textContent = "Copy"), 1500);
      } catch {
        toast("Copy failed: select the text by hand.", true);
      }
    },
  }, "Copy");
}

const app = document.getElementById("app");
const state = { status: null, view: "", members: [], sub: null };

function setView(...nodes) {
  app.replaceChildren(...nodes);
}

// ---------- Routing ----------

function route() {
  const hash = location.hash.replace(/^#\/?/, "");
  const [view, ...rest] = hash.split("/");
  return { view: view || "members", rest };
}

async function render() {
  if (document.body.classList.contains("login-mode")) return; // the login screen stays up
  const { view, rest } = route();
  state.view = view;
  document.getElementById("selbar")?.remove();
  for (const a of document.querySelectorAll("#nav a")) {
    a.classList.toggle("active", a.dataset.view === view || (view === "share" && a.dataset.view === "members"));
  }
  if (!state.status?.in_circle) return renderWelcome();
  if (view === "share") return renderShare(decodeURIComponent(rest[0] || ""), decodeURIComponent(rest.slice(1).join("/")));
  if (view === "downloads") return renderDownloads();
  if (view === "settings") return renderSettings();
  return renderMembers();
}

async function loadStatus() {
  state.status = await api("GET", "/status");
  const st = state.status;
  document.getElementById("nav").hidden = !st.in_circle;
  document.getElementById("whoami").textContent = st.self ? st.self.display_name + " · " + st.self.tunnel_ip : "";
  document.getElementById("logout").hidden = !st.password_set;
  updateStatusbar();
  const banner = document.getElementById("banner");
  banner.hidden = st.password_set;
  banner.textContent = st.password_set ? "" : "The interface is not protected: set UI_PASSWORD if other people use your local network.";
}

// ---------- Login ----------

function showLogin() {
  if (document.body.classList.contains("login-mode")) return;
  state.es?.close();
  state.es = null;
  document.body.classList.add("login-mode");
  const pw = h("input", { type: "password", id: "pw", name: "password", autocomplete: "current-password", required: true, "aria-label": "Password" });
  const toggle = h("button", {
    type: "button", class: "plain muted",
    onclick: () => {
      const show = pw.type === "password";
      pw.type = show ? "text" : "password";
      toggle.textContent = show ? "hide" : "show";
      pw.focus();
    },
  }, "show");
  const remember = h("input", { type: "checkbox", checked: true });
  const err = h("p", { class: "err", role: "alert" });
  const go = h("button", { class: "primary go", type: "submit" }, "login");
  const host = location.hostname || "cercle";
  const card = h("div", { class: "login" },
    h("div", { class: "bar-top" }, h("span", {}, "CERCLE"), h("span", {}, host)),
    h("div", { class: "body" },
      h("pre", { class: "banner-txt", "aria-hidden": "true" },
        "  ___ ___ ___  ___ _    ___\n / __| __| _ \\/ __| |  | __|\n| (__| _||   / (__| |__| _|\n \\___|___|_|_\\\\___|____|___|"),
      h("p", { class: "motd" }, "private sharing between friends, encrypted by WireGuard."),
      h("form", {
        onsubmit: async (ev) => {
          ev.preventDefault();
          err.textContent = "";
          go.disabled = true;
          go.textContent = "checking…";
          try {
            await api("POST", "/login", { password: pw.value, remember: remember.checked });
            document.body.classList.remove("login-mode");
            await boot();
          } catch (e) {
            err.textContent = e.message.toLowerCase().replace(/\.$/, "");
            card.classList.remove("shake");
            void card.offsetWidth; // restart the animation
            card.classList.add("shake");
            pw.select();
          } finally {
            go.disabled = false;
            go.textContent = "login";
          }
        },
      },
        // Hidden username: helps password managers store the login.
        h("input", { type: "text", name: "username", autocomplete: "username", value: "cercle", hidden: true }),
        h("div", { class: "prompt" }, h("label", { for: "pw", class: "cursor" }, "password "), pw, toggle),
        h("label", { class: "remember" }, remember, "stay signed in on this device (30 days)"),
        err,
        go),
      h("p", { class: "foot" }, "password: the container's UI_PASSWORD variable, or else the ui.password file in the /data volume.")));
  setView(card);
  setTimeout(() => pw.focus(), 50);
}

document.getElementById("logout").addEventListener("click", async () => {
  await api("POST", "/logout").catch(() => {});
  showLogin();
});

// ---------- Welcome (outside a circle) ----------

function renderWelcome() {
  const err = h("div", { class: "err" });
  const code = h("textarea", { placeholder: "cercle1:…", "aria-label": "Invitation code" });
  const joinBtn = h("button", {
    class: "primary",
    onclick: () => guard(joinBtn, async () => {
      err.textContent = "";
      if (!code.value.trim()) { err.textContent = "Paste the code you received first."; return; }
      joinBtn.textContent = "Contacting the inviter…";
      try {
        await api("POST", "/join", { code: code.value });
        await loadStatus();
        toast("Welcome to the circle.");
        location.hash = "#/members";
        render();
      } catch (e) {
        err.textContent = e.message;
      } finally {
        joinBtn.textContent = "Join";
      }
    }),
  }, "Join");
  const createBtn = h("button", {
    class: "primary",
    onclick: () => guard(createBtn, async () => {
      await api("POST", "/circle");
      await loadStatus();
      toast("Circle created. Invite your friends from the Members page.");
      render();
    }),
  }, "Create a circle");
  setView(
    h("div", { class: "hero" },
      h("h1", {}, "Welcome to Cercle"),
      h("p", { class: "muted" }, "This NAS does not belong to any circle yet. Create one if you are the first, or paste the invitation code a friend sent you.")),
    h("div", { class: "grid2" },
      h("div", { class: "card" },
        h("h3", {}, "Create a circle"),
        h("p", { class: "muted small" }, "You will be its first member, then you invite the others."),
        createBtn),
      h("div", { class: "card" },
        h("h3", {}, "Join with a code"),
        h("div", { class: "field" }, code, err),
        joinBtn)));
}

// ---------- Members ----------

async function renderMembers() {
  const invitePanel = h("div");
  const pending = h("div");
  const list = h("div", { class: "members" });
  const inviteBtn = h("button", {
    class: "primary",
    onclick: () => guard(inviteBtn, async () => {
      const inv = await api("POST", "/invites");
      const ta = h("textarea", { readonly: true, class: "invite-code", rows: 4 }, inv.code);
      invitePanel.replaceChildren(h("div", { class: "card" },
        h("h3", {}, "Invitation code"),
        h("p", { class: "small muted" }, "Send this code to your friend through a messaging app. It works once, until " + fmtDate(inv.expires_at) + ". Your friend pastes it into their Cercle interface, and that is all."),
        ta,
        h("div", { class: "actions" }, copyButton(() => inv.code), h("button", { onclick: () => invitePanel.replaceChildren() }, "Close"))));
      ta.select();
      loadPending();
    }),
  }, "invite a friend");

  async function loadPending() {
    const invs = (await api("GET", "/invites")).filter((i) => !i.used_at && toDate(i.expires_at) > new Date());
    pending.replaceChildren(...(invs.length ? [h("div", { class: "card" },
      h("h3", {}, "Pending invitations"),
      ...invs.map((i) => h("div", { class: "member pending-row" },
        h("div", { class: "who" },
          h("div", {}, "Invitation of " + fmtDate(i.created_at)),
          h("div", { class: "meta" }, h("span", {}, "expires " + fmtAgo(i.expires_at)), h("span", {}, "planned address " + i.assigned_ip))),
        h("button", {
          class: "danger",
          onclick: (ev) => guard(ev.target, async () => {
            await api("DELETE", "/invites/" + encodeURIComponent(i.id));
            toast("Invitation cancelled.");
            loadPending();
          }),
        }, "Cancel"))))] : []));
  }

  setView(
    h("div", { class: "section-head" }, h("h1", {}, "Members"), inviteBtn),
    invitePanel, pending, list);
  await Promise.all([loadMembers(list), loadPending().catch(() => {})]);
  state.refreshMembers = () => loadMembers(list);
}

async function loadMembers(list) {
  const members = await api("GET", "/members");
  state.members = members;
  updateStatusbar();
  const active = members.filter((m) => !m.revoked);
  const revoked = members.filter((m) => m.revoked);
  const head = h("div", { class: "mrow head" },
    h("span", {}, "state"), h("span", {}, "name"), h("span", {}, "address"), h("span", {}, "share"),
    h("span", {}, "seen"), h("span", {}, "endpoint"), h("span", {}, ""));
  const row = (m) => {
    let st;
    if (m.revoked) st = h("span", { class: "state-bad" }, "revoked");
    else if (m.self) st = h("span", { class: "state-on" }, "● local");
    else st = h("span", { class: m.online ? "state-on" : "state-off" }, m.online ? "● online" : "○ offline");
    const actions = [];
    if (!m.self && !m.revoked) {
      actions.push(h("a", { class: "btn", href: "#/share/" + encodeURIComponent(m.node_id) + "/" }, "browse"));
      actions.push(h("button", {
        class: "danger",
        onclick: (ev) => {
          if (!confirm("Revoke " + m.display_name + "? They will permanently lose access to the circle, at every member.")) return;
          guard(ev.target, async () => {
            await api("POST", "/members/" + encodeURIComponent(m.node_id) + "/revoke");
            toast(m.display_name + " has been revoked.");
            loadMembers(list);
          });
        },
      }, "revoke"));
    }
    const share = (m.share_files || m.self) ? fmtSize(m.share_size) + " · " + nf0.format(m.share_files) + " files" : "?";
    return h("div", { class: "mrow" },
      st,
      h("span", { class: "name" }, m.display_name, m.self && h("span", { class: "badge" }, "you"),
        m.conflict && h("span", { class: "badge bad" }, "address conflict")),
      h("span", { class: "col-ip mono" }, m.tunnel_ip),
      h("span", { class: "col-share" }, share),
      h("span", { class: "col-seen muted" }, m.self ? "" : (m.online ? "now" : fmtAgo(m.last_handshake))),
      h("span", { class: "col-ep mono muted" }, m.self ? "" : (m.endpoint || "")),
      h("span", { class: "actions" }, actions));
  };
  const nodes = [h("div", { class: "list cols-m" }, head, ...active.map(row))];
  if (active.length === 1) nodes.push(h("p", { class: "muted" }, "You are alone for now. Invite a friend to start sharing."));
  if (revoked.length) {
    nodes.push(h("details", { class: "members-revoked" }, h("summary", {}, plural(revoked.length, "revoked member", "revoked members")),
      h("div", { class: "list cols-m" }, ...revoked.map(row))));
  }
  list.replaceChildren(...nodes);
}

// ---------- A member's share ----------

const selections = new Map(); // node_id → Map(path → size)

async function renderShare(peerID, path) {
  path = path.replace(/\/+$/, "");
  if (!state.members.length) state.members = await api("GET", "/members").catch(() => []);
  const member = state.members.find((m) => m.node_id === peerID);
  const name = member ? member.display_name : "member";
  if (!selections.has(peerID)) selections.set(peerID, new Map());
  const sel = selections.get(peerID);

  const href = (p) => "#/share/" + encodeURIComponent(peerID) + "/" + p.split("/").map(encodeURIComponent).join("/");
  const parts = path ? path.split("/") : [];
  const crumbs = h("nav", { class: "crumbs", "aria-label": "Path" },
    h("a", { href: "#/members" }, "members"), h("span", { class: "sep" }, "/"),
    parts.length ? h("a", { href: href("") }, name) : h("span", { class: "here" }, name),
    parts.map((p, i) => [h("span", { class: "sep" }, "/"),
      i === parts.length - 1 ? h("span", { class: "here" }, p) : h("a", { href: href(parts.slice(0, i + 1).join("/")) }, p)]));
  const list = h("div", { class: "list" }, h("div", { class: "empty" }, "Reading " + name + "'s share…"));
  const more = h("div");
  setView(crumbs, list, more);

  const bar = h("div", { class: "selbar", id: "selbar", hidden: true });
  document.body.append(bar);
  const updateBar = () => {
    let total = 0;
    for (const s of sel.values()) total += s;
    bar.hidden = sel.size === 0;
    const go = h("button", {
      class: "primary",
      onclick: () => guard(go, async () => {
        const res = await api("POST", "/queue", { peer: peerID, paths: [...sel.keys()] });
        sel.clear();
        updateBar();
        for (const cb of list.querySelectorAll("input[type=checkbox]")) { cb.checked = false; cb.closest(".row")?.classList.remove("sel"); }
        const msg = res.added ? plural(res.added, "file", "files") + " queued." : "Nothing new: these files are already queued or present.";
        toast(res.skipped ? msg + " " + plural(res.skipped, "path refused", "paths refused") + "." : msg);
      }),
    }, "download");
    bar.replaceChildren(h("div", { class: "inner" },
      h("div", { class: "what" }, plural(sel.size, "item selected", "items selected") + ", " + fmtSize(total)),
      h("button", { onclick: () => { sel.clear(); updateBar(); for (const cb of list.querySelectorAll("input[type=checkbox]")) { cb.checked = false; cb.closest(".row")?.classList.remove("sel"); } } }, "clear selection"),
      go));
  };
  updateBar();

  const rows = [h("div", { class: "row head" },
    h("input", {
      type: "checkbox", "aria-label": "Select all",
      onchange: (ev) => {
        for (const cb of list.querySelectorAll(".row:not(.head) input[type=checkbox]")) {
          if (cb.checked !== ev.target.checked) cb.click();
        }
      },
    }),
    h("span", {}, "name"), h("span", { class: "num" }, "size"), h("span", { class: "num date" }, "modified"))];

  const addEntries = (entries) => {
    for (const e of entries) {
      const row = h("div", { class: "row" + (sel.has(e.path) ? " sel" : "") });
      const cb = h("input", {
        type: "checkbox", checked: sel.has(e.path), "aria-label": "Select " + e.name,
        onchange: () => {
          if (cb.checked) sel.set(e.path, e.size); else sel.delete(e.path);
          row.classList.toggle("sel", cb.checked);
          updateBar();
        },
      });
      const label = e.is_dir
        ? h("a", { href: href(e.path) }, e.name)
        : h("span", {}, e.name);
      row.append(cb,
        h("span", { class: "nm" }, h("span", { class: "ic " + (e.is_dir ? "dir" : "file"), "aria-hidden": "true" }), label,
          e.is_dir && h("span", { class: "muted small" }, plural(e.file_count || 0, "file", "files"))),
        h("span", { class: "num" }, fmtSize(e.size)),
        h("span", { class: "num date" }, fmtDate(e.mtime_ns)));
      rows.push(row);
    }
  };

  const load = async (cursor) => {
    try {
      const page = await api("GET", "/members/" + encodeURIComponent(peerID) + "/files?path=" + encodeURIComponent(path) + (cursor ? "&cursor=" + encodeURIComponent(cursor) : ""));
      addEntries(page.entries);
      list.replaceChildren(...rows);
      if (rows.length === 1) list.append(h("div", { class: "empty" }, "Empty folder."));
      more.replaceChildren(page.next ? h("p", {}, h("button", { onclick: () => load(page.next) }, "Show more")) : "");
    } catch (e) {
      list.replaceChildren(h("div", { class: "empty" }, e.message));
    }
  };
  load("");
}

// ---------- Downloads ----------

const FILTERS = [
  { key: "active", label: "Active and queued", status: "active,queued" },
  { key: "waiting", label: "Waiting", status: "waiting" },
  { key: "paused", label: "Paused", status: "paused" },
  { key: "errors", label: "Errors", status: "error" },
  { key: "done", label: "Done", status: "done" },
];
const STATE_LABEL = {
  queued: "queued", active: "active", paused: "paused", waiting: "waiting", done: "ok", error: "error",
};
const dlState = { filter: "active", limit: 200, selected: new Set(), updateBulk: null };

function countFor(sum, status) {
  return status.split(",").reduce((n, s) => n + (sum.count[s] || 0), 0);
}

async function renderDownloads() {
  const filters = h("div", { class: "filters" });
  const bulk = h("div", { class: "actions" });
  const list = h("div", { class: "list cols-d" });
  const more = h("div");
  const summary = h("p", { class: "muted small" });
  setView(h("div", { class: "section-head" }, h("h1", {}, "Downloads"), bulk), summary, filters, list, more);

  const load = async () => {
    const f = FILTERS.find((x) => x.key === dlState.filter);
    const data = await api("GET", "/queue?status=" + f.status + "&limit=" + dlState.limit);
    const sum = data.summary;
    const left = (sum.bytes.queued || 0) + (sum.bytes.active || 0) + (sum.bytes.waiting || 0);
    summary.textContent = left ? "Left to download: " + fmtSize(left) + "." : "Nothing to download for now.";
    filters.replaceChildren(...FILTERS.map((x) => h("button", {
      class: x.key === dlState.filter ? "on" : "",
      onclick: () => { dlState.filter = x.key; dlState.limit = 200; dlState.selected.clear(); load(); },
    }, x.label + " (" + nf0.format(countFor(sum, x.status)) + ")")));

    const sel = dlState.selected;
    const act = (action, label, extra) => h("button", {
      class: extra || "",
      onclick: (ev) => {
        let deletePart = false;
        if (action === "remove") {
          if (!confirm("Remove " + plural(sel.size, "item", "items") + " from the queue?")) return;
          deletePart = confirm("Also delete the partial files already received?");
        }
        guard(ev.target, async () => {
          await api("POST", "/queue/actions", { ids: [...sel], action, delete_part: deletePart });
          sel.clear();
          load();
        });
      },
    }, label);
    // Bulk actions: only shown when there is a selection.
    dlState.updateBulk = () => {
      const btns = [];
      if (sel.size) {
        btns.push(h("span", { class: "muted small" }, plural(sel.size, "selected", "selected")),
          act("pause", "Pause"), act("resume", "Resume"), act("priority", "Prioritize"), act("retry", "Retry"), act("remove", "Remove", "danger"));
      } else if (dlState.filter === "done" && countFor(sum, "done")) {
        btns.push(h("button", {
          onclick: (ev) => guard(ev.target, async () => {
            await api("POST", "/queue/actions", { action: "clear_done" });
            load();
          }),
        }, "Clear the list"));
      }
      bulk.replaceChildren(...btns);
    };
    dlState.updateBulk();

    const head = h("div", { class: "dl head" }, h("span", {}, ""), h("span", {}, "file"), h("span", {}, "progress"),
      h("span", { class: "num" }, "%"), h("span", { class: "num" }, "size"), h("span", {}, "state"), h("span", {}, ""));
    list.replaceChildren(...(data.items.length ? [head, ...data.items.map((it) => dlRow(it, load))] : [h("div", { class: "empty" }, "Nothing here.")]));
    more.replaceChildren(data.items.length >= dlState.limit
      ? h("p", {}, h("button", { onclick: () => { dlState.limit += 200; load(); } }, "Show more"))
      : "");
    updateNavCount(sum);
  };
  state.refreshQueue = debounce(load, 300);
  await load();
}

function dlRow(it, reload) {
  const name = it.remote_path.split("/").pop();
  const pct = it.size ? Math.min(100, (it.bytes_done / it.size) * 100) : (it.status === "done" ? 100 : 0);
  const fill = h("i");
  fill.style.width = pct.toFixed(1) + "%";
  const cb = h("input", {
    type: "checkbox", checked: dlState.selected.has(it.id), "aria-label": "Select " + name,
    onchange: () => {
      if (cb.checked) dlState.selected.add(it.id); else dlState.selected.delete(it.id);
      cb.closest(".dl").classList.toggle("sel", cb.checked);
      dlState.updateBulk?.();
    },
  });
  const one = (action, label, cls) => h("button", {
    class: cls || "",
    onclick: (ev) => guard(ev.target, async () => {
      if (action === "remove") {
        if (!confirm("Remove \"" + name + "\" from the queue?")) return;
        await api("DELETE", "/queue/" + it.id + (it.status !== "done" && confirm("Also delete the partial file?") ? "?part=1" : ""));
      } else {
        await api("PATCH", "/queue/" + it.id, { action });
      }
      reload();
    }),
  }, label);
  const actions = [];
  if (["active", "queued", "waiting"].includes(it.status)) {
    actions.push(one("pause", "pause"));
    if (!it.priority && it.status !== "active") actions.push(one("priority", "prio"));
  }
  if (it.status === "waiting") actions.push(one("resume", "retry now"));
  if (it.status === "paused") actions.push(one("resume", "resume"));
  if (it.status === "error") actions.push(one("retry", "retry"));
  actions.push(one("remove", "remove", "danger"));
  const [sizeTxt, stateTxt] = progressCols(it);
  const row = h("div", { class: "dl " + it.status + (dlState.selected.has(it.id) ? " sel" : ""), "data-id": it.id },
    cb,
    h("div", {},
      h("div", { class: "title" }, name, it.priority ? h("span", { class: "badge" }, "prio") : null),
      h("div", { class: "path" }, it.peer_name + ":" + it.remote_path)),
    h("div", { class: "bar" }, fill),
    h("span", { class: "num", "data-pct": "" }, Math.floor(pct) + "%"),
    h("span", { class: "num", "data-size": "" }, sizeTxt),
    h("span", { "data-state": "" }, stateTxt),
    h("span", { class: "actions" }, actions),
    it.last_error && it.status !== "done" ? h("div", { class: "err" }, it.last_error) : null);
  return row;
}

// progressCols returns the "size" and "state" columns.
function progressCols(it) {
  const size = it.status === "done" ? fmtSize(it.size) : fmtSize(it.bytes_done) + "/" + fmtSize(it.size);
  const st = [h("span", { class: "state " + it.status }, STATE_LABEL[it.status] || it.status)];
  if (it.status === "active" && it.rate > 0) {
    const left = (it.size - it.bytes_done) / it.rate;
    st.push(" " + fmtRate(it.rate) + (left >= 1 ? " " + fmtDuration(left) : ""));
  }
  if (it.status === "waiting" && it.next_attempt) st.push(" " + fmtAgo(it.next_attempt));
  if (it.attempts) st.push(" ×" + it.attempts);
  return [size, st];
}

// Live progress: updated in place, without redrawing everything. If the set of
// active transfers changed (a file finished, another started), the list is
// read again so statuses stay accurate.
let lastActive = "";
function applyProgress(items) {
  state.rate = items.reduce((t, it) => t + (it.rate || 0), 0);
  state.rateAt = Date.now();
  updateStatusbar();
  if (state.view !== "downloads") return;
  const ids = items.map((it) => it.id).sort().join(",");
  if (ids !== lastActive) {
    lastActive = ids;
    state.refreshQueue?.();
  }
  for (const it of items) {
    const row = document.querySelector('.dl[data-id="' + it.id + '"]');
    if (!row) continue;
    const pct = it.size ? Math.min(100, (it.bytes_done / it.size) * 100) : 0;
    row.querySelector(".bar > i").style.width = pct.toFixed(1) + "%";
    row.querySelector("[data-pct]").textContent = Math.floor(pct) + "%";
    const [sizeTxt, stateTxt] = progressCols(it);
    row.querySelector("[data-size]").textContent = sizeTxt;
    row.querySelector("[data-state]").replaceChildren(...stateTxt);
  }
}

function updateNavCount(sum) {
  const n = (sum.count.active || 0) + (sum.count.queued || 0) + (sum.count.waiting || 0);
  const el = document.getElementById("nav-count");
  el.hidden = !n;
  el.textContent = nf0.format(n);
  state.queueCount = n;
  updateStatusbar();
}

// Bottom status bar: online members, queue, download rate.
function updateStatusbar() {
  const bar = document.getElementById("statusbar");
  if (!state.status?.in_circle) { bar.hidden = true; return; }
  bar.hidden = false;
  const others = state.members.filter((m) => !m.self && !m.revoked);
  document.getElementById("sb-online").textContent = others.filter((m) => m.online).length + "/" + others.length;
  document.getElementById("sb-queue").textContent = nf0.format(state.queueCount || 0);
  const fresh = state.rateAt && Date.now() - state.rateAt < 2500;
  document.getElementById("sb-rate").textContent = fmtRate(fresh ? state.rate : 0);
  document.getElementById("sb-version").textContent = "cercle " + (state.status.version || "");
}
setInterval(updateStatusbar, 3000); // the rate drops to 0 when updates stop

// ---------- Settings ----------

async function renderSettings() {
  const [settings, share] = await Promise.all([api("GET", "/settings"), api("GET", "/share")]);
  const st = state.status;
  const inputs = {};
  const errs = {};
  const field = (key, label, hint, type) => {
    inputs[key] = h("input", { type: type || "text", id: "f-" + key, value: String(settings[key] ?? ""), min: type === "number" ? 1 : null });
    errs[key] = h("div", { class: "err" });
    return h("div", { class: "field" }, h("label", { for: "f-" + key }, label), inputs[key], hint && h("div", { class: "hint" }, hint), errs[key]);
  };
  const saveBtn = h("button", {
    class: "primary",
    onclick: () => guard(saveBtn, async () => {
      for (const e of Object.values(errs)) e.textContent = "";
      const body = {};
      for (const [k, el] of Object.entries(inputs)) body[k] = el.type === "number" ? Number(el.value) : el.value;
      try {
        await api("PUT", "/settings", body);
        toast("Settings saved and applied.");
        await loadStatus();
      } catch (e) {
        if (e.fields) for (const [k, msg] of Object.entries(e.fields)) if (errs[k]) errs[k].textContent = msg;
        throw e;
      }
    }),
  }, "Save");
  const rateHint = "Bytes per second, with K, M or G suffix (20M = 20 MB/s). 0 = unlimited.";
  const winHint = "Daily window, for example 01:00-07:00. Empty = any time.";
  setView(
    h("h1", {}, "Settings"),
    h("p", { class: "muted small" }, "These settings apply immediately and then take precedence over the container's environment variables."),
    h("div", { class: "card" },
      h("h3", {}, "This node"),
      field("name", "Name shown to other members")),
    h("div", { class: "grid2" },
      h("div", { class: "card" },
        h("h3", {}, "Upload to others"),
        field("upload_limit", "Maximum rate", rateHint),
        field("max_uploads", "Concurrent uploads", null, "number"),
        field("upload_window", "Upload window", winHint)),
      h("div", { class: "card" },
        h("h3", {}, "Download"),
        field("download_limit", "Maximum rate", rateHint),
        field("download_concurrency", "Concurrent downloads", "At most one at a time per member.", "number"),
        field("download_window", "Download window", winHint))),
    h("p", {}, saveBtn),
    h("div", { class: "grid2" },
      sharePanel(share),
      h("div", { class: "card" },
        h("h3", {}, "Network"),
        h("dl", { class: "kv" },
          h("dt", {}, "Tunnel address"), h("dd", { class: "mono" }, st.self?.tunnel_ip || ""),
          h("dt", {}, "UDP port"), h("dd", {}, String(st.listen_port)),
          h("dt", {}, "Announced endpoints"), h("dd", { class: "mono" }, st.announced_endpoints?.length ? st.announced_endpoints.join(", ") : "none"),
          h("dt", {}, "Public key"), h("dd", { class: "mono" }, st.public_key),
          h("dt", {}, "Circle"), h("dd", { class: "mono" }, st.circle_id + " (" + st.subnet + ")"),
          h("dt", {}, "Version"), h("dd", {}, st.version)),
        !st.announced_endpoints?.length && h("p", { class: "hint" }, "Without an announced endpoint (WG_PUBLIC_ENDPOINTS), others only reach you if you contact them first, and you can only invite if a member already sees you."),
        h("p", {}, copyButton(() => st.public_key)))));
}

// ---------- Share: choosing the top-level folders of /share ----------

// sharePanel shows what /share contains and what of it is shared. Selection
// only applies to the top level (the mounted volumes); everything below
// follows its folder.
function sharePanel(share) {
  const card = h("div", { class: "card share-panel" });
  const draw = (share) => {
    const all = h("input", { type: "checkbox", checked: share.all });
    const boxes = new Map();
    const rows = share.folders.map((f) => {
      const cb = h("input", { type: "checkbox", checked: f.shared, disabled: share.all, "aria-label": "Share " + f.name });
      boxes.set(f.name, cb);
      return h("label", { class: "share-row" + (f.shared ? " on" : "") },
        cb,
        h("span", { class: "ic " + (f.is_dir ? "dir" : "file"), "aria-hidden": "true" }),
        h("span", { class: "nm" }, f.name),
        h("span", { class: "num" }, f.shared ? fmtSize(f.size) : ""),
        h("span", { class: "num muted" }, f.shared ? nf0.format(f.files) + " files" : "not shared"));
    });
    all.addEventListener("change", () => { for (const cb of boxes.values()) { cb.disabled = all.checked; if (all.checked) cb.checked = true; } });
    const apply = h("button", {
      class: "primary",
      onclick: () => guard(apply, async () => {
        const folders = [...boxes].filter(([, cb]) => cb.checked).map(([n]) => n);
        const res = await api("PUT", "/share", { all: all.checked, folders });
        draw(res);
        toast("Share updated, reindexing.");
      }),
    }, "apply");
    const rescan = h("button", {
      onclick: () => guard(rescan, async () => {
        await api("POST", "/scan");
        toast("Share scan started.");
      }),
    }, "rescan");
    card.replaceChildren(
      h("h3", {}, "Share"),
      h("dl", { class: "kv" },
        h("dt", {}, "Folder"), h("dd", { class: "mono" }, share.dir),
        h("dt", {}, "Shared"), h("dd", {}, plural(share.files, "file", "files") + ", " + fmtSize(share.size)),
        h("dt", {}, "Last scan"), h("dd", {}, share.last_scan ? fmtAgo(share.last_scan) : "never"),
        h("dt", {}, "Active uploads"), h("dd", {}, share.uploads_active + " of " + share.uploads_max)),
      h("label", { class: "share-all" }, all, "share everything, including folders mounted later"),
      h("div", { class: "share-list" }, rows.length ? rows : h("div", { class: "empty" }, "/share is empty: mount your folders there in the compose file.")),
      h("p", { class: "hint" }, "Only the top level of /share is chosen here. To add a folder, mount it in the compose file, for example /mnt/nas/movies:/share/movies:ro."),
      h("div", { class: "actions" }, apply, rescan));
  };
  draw(share);
  state.refreshShare = async () => { if (card.isConnected) draw(await api("GET", "/share")); };
  return card;
}

// ---------- Live events ----------

function debounce(fn, ms) {
  let t;
  return () => { clearTimeout(t); t = setTimeout(fn, ms); };
}

// Members read again without rendering, for the status bar of other views.
function refreshMembersQuietly() {
  if (!state.status?.in_circle) return;
  api("GET", "/members").then((m) => { state.members = m; updateStatusbar(); }).catch(() => {});
}

function connectEvents() {
  state.es?.close();
  const es = new EventSource("/ui/api/events");
  state.es = es;
  es.addEventListener("progress", (ev) => applyProgress(JSON.parse(ev.data)));
  es.addEventListener("queue", () => {
    if (state.view === "downloads" && state.refreshQueue) state.refreshQueue();
    else api("GET", "/queue?limit=1").then((d) => updateNavCount(d.summary)).catch(() => {});
  });
  es.addEventListener("members", debounce(() => {
    if (state.view === "members" && state.refreshMembers) state.refreshMembers().catch(() => {});
    else refreshMembersQuietly();
  }, 500));
  es.addEventListener("settings", () => loadStatus().catch(() => {}));
  es.addEventListener("share", () => { if (state.view === "settings") state.refreshShare?.().catch(() => {}); });
}

// ---------- Startup ----------

// Keyboard shortcuts: 1, 2, 3 for the views, ? for help.
document.addEventListener("keydown", (ev) => {
  if (ev.ctrlKey || ev.metaKey || ev.altKey || document.body.classList.contains("login-mode")) return;
  if (["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName)) return;
  const views = { "1": "#/members", "2": "#/downloads", "3": "#/settings" };
  if (views[ev.key] && state.status?.in_circle) { location.hash = views[ev.key]; ev.preventDefault(); }
  if (ev.key === "?") toast("shortcuts: 1 members · 2 downloads · 3 settings");
});

window.addEventListener("hashchange", () => render().catch((e) => { if (!e.login) toast(e.message, true); }));

async function boot() {
  try {
    await loadStatus();
    await render();
    if (state.status.in_circle) {
      api("GET", "/queue?limit=1").then((d) => updateNavCount(d.summary)).catch(() => {});
      refreshMembersQuietly();
    }
    connectEvents();
  } catch (e) {
    if (e.login) return; // the login page is shown
    setView(h("div", { class: "card" }, h("h3", {}, "Interface unavailable"), h("p", { class: "err" }, e.message)));
  }
}

boot();
