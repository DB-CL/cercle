"use strict";

// Interface de Cercle. Tout texte venu d'un pair (noms, chemins, messages)
// est inséré comme texte, jamais comme HTML.

// ---------- Outils ----------

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
    const e = new Error("Connexion requise.");
    e.login = true;
    throw e;
  }
  if (!r.ok) {
    const e = new Error(data.error || "Erreur " + r.status);
    e.status = r.status;
    e.fields = data.fields;
    throw e;
  }
  return data;
}

const nf1 = new Intl.NumberFormat("fr-FR", { maximumFractionDigits: 1 });
const nf0 = new Intl.NumberFormat("fr-FR", { maximumFractionDigits: 0 });

function fmtSize(n) {
  n = Number(n) || 0;
  const units = ["o", "Ko", "Mo", "Go", "To"];
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
  return d ? d.toLocaleString("fr-FR", { dateStyle: "short", timeStyle: "short" }) : "";
}
function fmtAgo(v) {
  const d = toDate(v);
  if (!d) return "jamais";
  const s = Math.round((Date.now() - d) / 1000);
  if (s < 0) return "dans " + fmtDuration(-s);
  if (s < 45) return "à l'instant";
  return "il y a " + fmtDuration(s);
}
function fmtDuration(s) {
  s = Math.max(0, Math.round(s));
  if (s < 60) return s + " s";
  if (s < 3600) return Math.round(s / 60) + " min";
  if (s < 86400) {
    const hh = Math.floor(s / 3600), mm = Math.round((s % 3600) / 60);
    return hh + " h " + String(mm).padStart(2, "0");
  }
  return Math.round(s / 86400) + " j";
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
        ev.target.textContent = "Copié";
        setTimeout(() => (ev.target.textContent = "Copier"), 1500);
      } catch {
        toast("Copie impossible : sélectionnez le texte à la main.", true);
      }
    },
  }, "Copier");
}

const app = document.getElementById("app");
const state = { status: null, view: "", members: [], sub: null };

function setView(...nodes) {
  app.replaceChildren(...nodes);
}

// ---------- Routage ----------

function route() {
  const hash = location.hash.replace(/^#\/?/, "");
  const [view, ...rest] = hash.split("/");
  return { view: view || "membres", rest };
}

async function render() {
  if (document.body.classList.contains("login-mode")) return; // l'écran de connexion reste affiché
  const { view, rest } = route();
  state.view = view;
  document.getElementById("selbar")?.remove();
  for (const a of document.querySelectorAll("#nav a")) {
    a.classList.toggle("active", a.dataset.view === view || (view === "partage" && a.dataset.view === "membres"));
  }
  if (!state.status?.in_circle) return renderWelcome();
  if (view === "partage") return renderShare(decodeURIComponent(rest[0] || ""), decodeURIComponent(rest.slice(1).join("/")));
  if (view === "telechargements") return renderDownloads();
  if (view === "reglages") return renderSettings();
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
  banner.textContent = st.password_set ? "" : "L'interface n'est pas protégée : définissez UI_PASSWORD si d'autres personnes utilisent votre réseau local.";
}

// ---------- Connexion ----------

function showLogin() {
  if (document.body.classList.contains("login-mode")) return;
  state.es?.close();
  state.es = null;
  document.body.classList.add("login-mode");
  const pw = h("input", { type: "password", id: "pw", name: "password", autocomplete: "current-password", required: true, "aria-label": "Mot de passe" });
  const toggle = h("button", {
    type: "button", class: "plain muted",
    onclick: () => {
      const show = pw.type === "password";
      pw.type = show ? "text" : "password";
      toggle.textContent = show ? "masquer" : "afficher";
      pw.focus();
    },
  }, "afficher");
  const remember = h("input", { type: "checkbox", checked: true });
  const err = h("p", { class: "err", role: "alert" });
  const go = h("button", { class: "primary go", type: "submit" }, "connexion");
  const host = location.hostname || "cercle";
  const card = h("div", { class: "login" },
    h("div", { class: "bar-top" }, h("span", {}, "CERCLE"), h("span", {}, host)),
    h("div", { class: "body" },
      h("pre", { class: "banner-txt", "aria-hidden": "true" },
        "  ___ ___ ___  ___ _    ___\n / __| __| _ \\/ __| |  | __|\n| (__| _||   / (__| |__| _|\n \\___|___|_|_\\\\___|____|___|"),
      h("p", { class: "motd" }, "partage privé entre amis, chiffré par WireGuard."),
      h("form", {
        onsubmit: async (ev) => {
          ev.preventDefault();
          err.textContent = "";
          go.disabled = true;
          go.textContent = "vérification…";
          try {
            await api("POST", "/login", { password: pw.value, remember: remember.checked });
            document.body.classList.remove("login-mode");
            await boot();
          } catch (e) {
            err.textContent = e.message.toLowerCase().replace(/\.$/, "");
            card.classList.remove("shake");
            void card.offsetWidth; // relance l'animation
            card.classList.add("shake");
            pw.select();
          } finally {
            go.disabled = false;
            go.textContent = "connexion";
          }
        },
      },
        // Nom d'utilisateur invisible : aide les gestionnaires de mots de passe à enregistrer l'accès.
        h("input", { type: "text", name: "username", autocomplete: "username", value: "cercle", hidden: true }),
        h("div", { class: "prompt" }, h("label", { for: "pw", class: "cursor" }, "mot de passe "), pw, toggle),
        h("label", { class: "remember" }, remember, "rester connecté sur cet appareil (30 jours)"),
        err,
        go),
      h("p", { class: "foot" }, "le mot de passe est celui de la variable UI_PASSWORD du conteneur.")));
  setView(card);
  setTimeout(() => pw.focus(), 50);
}

document.getElementById("logout").addEventListener("click", async () => {
  await api("POST", "/logout").catch(() => {});
  showLogin();
});

// ---------- Accueil (hors cercle) ----------

function renderWelcome() {
  const err = h("div", { class: "err" });
  const code = h("textarea", { placeholder: "cercle1:…", "aria-label": "Code d'invitation" });
  const joinBtn = h("button", {
    class: "primary",
    onclick: () => guard(joinBtn, async () => {
      err.textContent = "";
      if (!code.value.trim()) { err.textContent = "Collez d'abord le code reçu."; return; }
      joinBtn.textContent = "Connexion à l'invitant…";
      try {
        await api("POST", "/join", { code: code.value });
        await loadStatus();
        toast("Bienvenue dans le cercle.");
        location.hash = "#/membres";
        render();
      } catch (e) {
        err.textContent = e.message;
      } finally {
        joinBtn.textContent = "Rejoindre";
      }
    }),
  }, "Rejoindre");
  const createBtn = h("button", {
    class: "primary",
    onclick: () => guard(createBtn, async () => {
      await api("POST", "/circle");
      await loadStatus();
      toast("Cercle créé. Invitez vos amis depuis la page Membres.");
      render();
    }),
  }, "Créer un cercle");
  setView(
    h("div", { class: "hero" },
      h("h1", {}, "Bienvenue sur Cercle"),
      h("p", { class: "muted" }, "Ce NAS n'appartient encore à aucun cercle. Créez-en un si vous êtes le premier, ou collez le code d'invitation qu'un ami vous a envoyé.")),
    h("div", { class: "grid2" },
      h("div", { class: "card" },
        h("h3", {}, "Créer un cercle"),
        h("p", { class: "muted small" }, "Vous en serez le premier membre, puis vous inviterez les autres."),
        createBtn),
      h("div", { class: "card" },
        h("h3", {}, "Rejoindre avec un code"),
        h("div", { class: "field" }, code, err),
        joinBtn)));
}

// ---------- Membres ----------

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
        h("h3", {}, "Code d'invitation"),
        h("p", { class: "small muted" }, "Envoyez ce code à votre ami par messagerie. Il sert une seule fois, jusqu'au " + fmtDate(inv.expires_at) + ". Votre ami le colle dans son interface Cercle, et c'est tout."),
        ta,
        h("div", { class: "actions" }, copyButton(() => inv.code), h("button", { onclick: () => invitePanel.replaceChildren() }, "Fermer"))));
      ta.select();
      loadPending();
    }),
  }, "inviter un ami");

  async function loadPending() {
    const invs = (await api("GET", "/invites")).filter((i) => !i.used_at && toDate(i.expires_at) > new Date());
    pending.replaceChildren(...(invs.length ? [h("div", { class: "card" },
      h("h3", {}, "Invitations en attente"),
      ...invs.map((i) => h("div", { class: "member pending-row" },
        h("div", { class: "who" },
          h("div", {}, "Invitation du " + fmtDate(i.created_at)),
          h("div", { class: "meta" }, h("span", {}, "expire " + fmtAgo(i.expires_at)), h("span", {}, "adresse prévue " + i.assigned_ip))),
        h("button", {
          class: "danger",
          onclick: (ev) => guard(ev.target, async () => {
            await api("DELETE", "/invites/" + encodeURIComponent(i.id));
            toast("Invitation annulée.");
            loadPending();
          }),
        }, "Annuler"))))] : []));
  }

  setView(
    h("div", { class: "section-head" }, h("h1", {}, "Membres"), inviteBtn),
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
    h("span", {}, "état"), h("span", {}, "nom"), h("span", {}, "adresse"), h("span", {}, "partage"),
    h("span", {}, "vu"), h("span", {}, "endpoint"), h("span", {}, ""));
  const row = (m) => {
    let st;
    if (m.revoked) st = h("span", { class: "state-bad" }, "révoqué");
    else if (m.self) st = h("span", { class: "state-on" }, "● local");
    else st = h("span", { class: m.online ? "state-on" : "state-off" }, m.online ? "● actif" : "○ absent");
    const actions = [];
    if (!m.self && !m.revoked) {
      actions.push(h("a", { class: "btn", href: "#/partage/" + encodeURIComponent(m.node_id) + "/" }, "parcourir"));
      actions.push(h("button", {
        class: "danger",
        onclick: (ev) => {
          if (!confirm("Révoquer " + m.display_name + " ? Il perdra définitivement l'accès au cercle, chez tous les membres.")) return;
          guard(ev.target, async () => {
            await api("POST", "/members/" + encodeURIComponent(m.node_id) + "/revoke");
            toast(m.display_name + " a été révoqué.");
            loadMembers(list);
          });
        },
      }, "révoquer"));
    }
    const share = (m.share_files || m.self) ? fmtSize(m.share_size) + " · " + nf0.format(m.share_files) + " fich." : "?";
    return h("div", { class: "mrow" },
      st,
      h("span", { class: "name" }, m.display_name, m.self && h("span", { class: "badge" }, "vous"),
        m.conflict && h("span", { class: "badge bad" }, "conflit d'adresse")),
      h("span", { class: "col-ip mono" }, m.tunnel_ip),
      h("span", { class: "col-share" }, share),
      h("span", { class: "col-seen muted" }, m.self ? "" : (m.online ? "maintenant" : fmtAgo(m.last_handshake))),
      h("span", { class: "col-ep mono muted" }, m.self ? "" : (m.endpoint || "")),
      h("span", { class: "actions" }, actions));
  };
  const nodes = [h("div", { class: "list cols-m" }, head, ...active.map(row))];
  if (active.length === 1) nodes.push(h("p", { class: "muted" }, "Vous êtes seul pour l'instant. Invitez un ami pour commencer à échanger."));
  if (revoked.length) {
    nodes.push(h("details", { class: "members-revoked" }, h("summary", {}, plural(revoked.length, "membre révoqué", "membres révoqués")),
      h("div", { class: "list cols-m" }, ...revoked.map(row))));
  }
  list.replaceChildren(...nodes);
}

// ---------- Partage d'un membre ----------

const selections = new Map(); // node_id → Map(chemin → taille)

async function renderShare(peerID, path) {
  path = path.replace(/\/+$/, "");
  if (!state.members.length) state.members = await api("GET", "/members").catch(() => []);
  const member = state.members.find((m) => m.node_id === peerID);
  const name = member ? member.display_name : "membre";
  if (!selections.has(peerID)) selections.set(peerID, new Map());
  const sel = selections.get(peerID);

  const href = (p) => "#/partage/" + encodeURIComponent(peerID) + "/" + p.split("/").map(encodeURIComponent).join("/");
  const parts = path ? path.split("/") : [];
  const crumbs = h("nav", { class: "crumbs", "aria-label": "Chemin" },
    h("a", { href: "#/membres" }, "membres"), h("span", { class: "sep" }, "/"),
    parts.length ? h("a", { href: href("") }, name) : h("span", { class: "here" }, name),
    parts.map((p, i) => [h("span", { class: "sep" }, "/"),
      i === parts.length - 1 ? h("span", { class: "here" }, p) : h("a", { href: href(parts.slice(0, i + 1).join("/")) }, p)]));
  const list = h("div", { class: "list" }, h("div", { class: "empty" }, "Lecture du partage de " + name + "…"));
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
        const msg = res.added ? plural(res.added, "fichier ajouté", "fichiers ajoutés") + " à la file." : "Rien de nouveau : ces fichiers sont déjà en file ou présents.";
        toast(res.skipped ? msg + " " + plural(res.skipped, "chemin refusé", "chemins refusés") + "." : msg);
      }),
    }, "télécharger");
    bar.replaceChildren(h("div", { class: "inner" },
      h("div", { class: "what" }, plural(sel.size, "élément sélectionné", "éléments sélectionnés") + ", " + fmtSize(total)),
      h("button", { onclick: () => { sel.clear(); updateBar(); for (const cb of list.querySelectorAll("input[type=checkbox]")) { cb.checked = false; cb.closest(".row")?.classList.remove("sel"); } } }, "désélectionner"),
      go));
  };
  updateBar();

  const rows = [h("div", { class: "row head" },
    h("input", {
      type: "checkbox", "aria-label": "Tout sélectionner",
      onchange: (ev) => {
        for (const cb of list.querySelectorAll(".row:not(.head) input[type=checkbox]")) {
          if (cb.checked !== ev.target.checked) cb.click();
        }
      },
    }),
    h("span", {}, "nom"), h("span", { class: "num" }, "taille"), h("span", { class: "num date" }, "modifié"))];

  const addEntries = (entries) => {
    for (const e of entries) {
      const row = h("div", { class: "row" + (sel.has(e.path) ? " sel" : "") });
      const cb = h("input", {
        type: "checkbox", checked: sel.has(e.path), "aria-label": "Sélectionner " + e.name,
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
          e.is_dir && h("span", { class: "muted small" }, plural(e.file_count || 0, "fichier", "fichiers"))),
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
      if (rows.length === 1) list.append(h("div", { class: "empty" }, "Dossier vide."));
      more.replaceChildren(page.next ? h("p", {}, h("button", { onclick: () => load(page.next) }, "Afficher la suite")) : "");
    } catch (e) {
      list.replaceChildren(h("div", { class: "empty" }, e.message));
    }
  };
  load("");
}

// ---------- Téléchargements ----------

const FILTERS = [
  { key: "en-cours", label: "En cours et en file", status: "active,queued" },
  { key: "attente", label: "En attente", status: "waiting" },
  { key: "pause", label: "Suspendus", status: "paused" },
  { key: "erreurs", label: "Erreurs", status: "error" },
  { key: "termines", label: "Terminés", status: "done" },
];
const STATE_LABEL = {
  queued: "file", active: "actif", paused: "pause", waiting: "attente", done: "ok", error: "erreur",
};
const dlState = { filter: "en-cours", limit: 200, selected: new Set(), updateBulk: null };

function countFor(sum, status) {
  return status.split(",").reduce((n, s) => n + (sum.count[s] || 0), 0);
}

async function renderDownloads() {
  const filters = h("div", { class: "filters" });
  const bulk = h("div", { class: "actions" });
  const list = h("div", { class: "list cols-d" });
  const more = h("div");
  const summary = h("p", { class: "muted small" });
  setView(h("div", { class: "section-head" }, h("h1", {}, "Téléchargements"), bulk), summary, filters, list, more);

  const load = async () => {
    const f = FILTERS.find((x) => x.key === dlState.filter);
    const data = await api("GET", "/queue?status=" + f.status + "&limit=" + dlState.limit);
    const sum = data.summary;
    const left = (sum.bytes.queued || 0) + (sum.bytes.active || 0) + (sum.bytes.waiting || 0);
    summary.textContent = left ? "Reste à télécharger : " + fmtSize(left) + "." : "Rien à télécharger pour l'instant.";
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
          if (!confirm("Retirer " + plural(sel.size, "élément", "éléments") + " de la file ?")) return;
          deletePart = confirm("Supprimer aussi les fichiers partiels déjà reçus ?");
        }
        guard(ev.target, async () => {
          await api("POST", "/queue/actions", { ids: [...sel], action, delete_part: deletePart });
          sel.clear();
          load();
        });
      },
    }, label);
    // Actions groupées : visibles seulement quand une sélection existe.
    dlState.updateBulk = () => {
      const btns = [];
      if (sel.size) {
        btns.push(h("span", { class: "muted small" }, plural(sel.size, "sélectionné", "sélectionnés")),
          act("pause", "Suspendre"), act("resume", "Reprendre"), act("priority", "Prioritaire"), act("retry", "Relancer"), act("remove", "Retirer", "danger"));
      } else if (dlState.filter === "termines" && countFor(sum, "done")) {
        btns.push(h("button", {
          onclick: (ev) => guard(ev.target, async () => {
            await api("POST", "/queue/actions", { action: "clear_done" });
            load();
          }),
        }, "Vider la liste"));
      }
      bulk.replaceChildren(...btns);
    };
    dlState.updateBulk();

    const head = h("div", { class: "dl head" }, h("span", {}, ""), h("span", {}, "fichier"), h("span", {}, "progression"),
      h("span", { class: "num" }, "%"), h("span", { class: "num" }, "taille"), h("span", {}, "état"), h("span", {}, ""));
    list.replaceChildren(...(data.items.length ? [head, ...data.items.map((it) => dlRow(it, load))] : [h("div", { class: "empty" }, "Aucun élément ici.")]));
    more.replaceChildren(data.items.length >= dlState.limit
      ? h("p", {}, h("button", { onclick: () => { dlState.limit += 200; load(); } }, "Afficher plus"))
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
    type: "checkbox", checked: dlState.selected.has(it.id), "aria-label": "Sélectionner " + name,
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
        if (!confirm("Retirer « " + name + " » de la file ?")) return;
        await api("DELETE", "/queue/" + it.id + (it.status !== "done" && confirm("Supprimer aussi le fichier partiel ?") ? "?part=1" : ""));
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
  if (it.status === "waiting") actions.push(one("resume", "réessayer"));
  if (it.status === "paused") actions.push(one("resume", "reprendre"));
  if (it.status === "error") actions.push(one("retry", "relancer"));
  actions.push(one("remove", "retirer", "danger"));
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

// progressCols renvoie la colonne « taille » et la colonne « état ».
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

// Progression en direct : mise à jour sur place, sans tout redessiner. Si
// l'ensemble des transferts actifs a changé (un fichier fini, un autre
// commencé), la liste est relue pour que les statuts restent exacts.
let lastActive = "";
function applyProgress(items) {
  state.rate = items.reduce((t, it) => t + (it.rate || 0), 0);
  state.rateAt = Date.now();
  updateStatusbar();
  if (state.view !== "telechargements") return;
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

// Barre d'état du bas : membres en ligne, file, débit de réception.
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
setInterval(updateStatusbar, 3000); // le débit retombe à 0 quand les mises à jour cessent

// ---------- Réglages ----------

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
        toast("Réglages enregistrés et appliqués.");
        await loadStatus();
      } catch (e) {
        if (e.fields) for (const [k, msg] of Object.entries(e.fields)) if (errs[k]) errs[k].textContent = msg;
        throw e;
      }
    }),
  }, "Enregistrer");
  const rateHint = "Octets par seconde, suffixes K, M ou G (20M = 20 Mo/s). 0 = illimité.";
  const winHint = "Créneau quotidien, par exemple 01:00-07:00. Vide = à toute heure.";
  setView(
    h("h1", {}, "Réglages"),
    h("p", { class: "muted small" }, "Ces réglages s'appliquent tout de suite et priment ensuite sur les variables d'environnement du conteneur."),
    h("div", { class: "card" },
      h("h3", {}, "Ce nœud"),
      field("name", "Nom affiché aux autres membres")),
    h("div", { class: "grid2" },
      h("div", { class: "card" },
        h("h3", {}, "Envoi vers les autres"),
        field("upload_limit", "Débit maximal", rateHint),
        field("max_uploads", "Envois simultanés", null, "number"),
        field("upload_window", "Créneau d'envoi", winHint)),
      h("div", { class: "card" },
        h("h3", {}, "Réception"),
        field("download_limit", "Débit maximal", rateHint),
        field("download_concurrency", "Téléchargements simultanés", "Au plus un à la fois par membre.", "number"),
        field("download_window", "Créneau de téléchargement", winHint))),
    h("p", {}, saveBtn),
    h("div", { class: "grid2" },
      sharePanel(share),
      h("div", { class: "card" },
        h("h3", {}, "Réseau"),
        h("dl", { class: "kv" },
          h("dt", {}, "Adresse de tunnel"), h("dd", { class: "mono" }, st.self?.tunnel_ip || ""),
          h("dt", {}, "Port UDP"), h("dd", {}, String(st.listen_port)),
          h("dt", {}, "Endpoints annoncés"), h("dd", { class: "mono" }, st.announced_endpoints?.length ? st.announced_endpoints.join(", ") : "aucun"),
          h("dt", {}, "Clé publique"), h("dd", { class: "mono" }, st.public_key),
          h("dt", {}, "Cercle"), h("dd", { class: "mono" }, st.circle_id + " (" + st.subnet + ")"),
          h("dt", {}, "Version"), h("dd", {}, st.version)),
        !st.announced_endpoints?.length && h("p", { class: "hint" }, "Sans endpoint annoncé (WG_PUBLIC_ENDPOINTS), les autres ne vous joignent que si vous les contactez d'abord, et vous ne pouvez inviter que si un membre vous voit déjà."),
        h("p", {}, copyButton(() => st.public_key)))));
}

// ---------- Partage : choix des dossiers du premier niveau de /share ----------

// sharePanel affiche ce que contient /share et ce qui en est partagé. La
// sélection ne porte que sur le premier niveau (les volumes montés) ; tout ce
// qui est dessous suit son dossier.
function sharePanel(share) {
  const card = h("div", { class: "card share-panel" });
  const draw = (share) => {
    const all = h("input", { type: "checkbox", checked: share.all });
    const boxes = new Map();
    const rows = share.folders.map((f) => {
      const cb = h("input", { type: "checkbox", checked: f.shared, disabled: share.all, "aria-label": "Partager " + f.name });
      boxes.set(f.name, cb);
      return h("label", { class: "share-row" + (f.shared ? " on" : "") },
        cb,
        h("span", { class: "ic " + (f.is_dir ? "dir" : "file"), "aria-hidden": "true" }),
        h("span", { class: "nm" }, f.name),
        h("span", { class: "num" }, f.shared ? fmtSize(f.size) : ""),
        h("span", { class: "num muted" }, f.shared ? nf0.format(f.files) + " fich." : "non partagé"));
    });
    all.addEventListener("change", () => { for (const cb of boxes.values()) { cb.disabled = all.checked; if (all.checked) cb.checked = true; } });
    const apply = h("button", {
      class: "primary",
      onclick: () => guard(apply, async () => {
        const folders = [...boxes].filter(([, cb]) => cb.checked).map(([n]) => n);
        const res = await api("PUT", "/share", { all: all.checked, folders });
        draw(res);
        toast("Partage mis à jour, réindexation en cours.");
      }),
    }, "appliquer");
    const rescan = h("button", {
      onclick: () => guard(rescan, async () => {
        await api("POST", "/scan");
        toast("Scan du partage lancé.");
      }),
    }, "relancer le scan");
    card.replaceChildren(
      h("h3", {}, "Partage"),
      h("dl", { class: "kv" },
        h("dt", {}, "Dossier"), h("dd", { class: "mono" }, share.dir),
        h("dt", {}, "Partagé"), h("dd", {}, plural(share.files, "fichier", "fichiers") + ", " + fmtSize(share.size)),
        h("dt", {}, "Dernier scan"), h("dd", {}, share.last_scan ? fmtAgo(share.last_scan) : "aucun"),
        h("dt", {}, "Envois en cours"), h("dd", {}, share.uploads_active + " sur " + share.uploads_max)),
      h("label", { class: "share-all" }, all, "tout partager, y compris les dossiers montés plus tard"),
      h("div", { class: "share-list" }, rows.length ? rows : h("div", { class: "empty" }, "/share est vide : montez-y vos dossiers dans le compose.")),
      h("p", { class: "hint" }, "Seul le premier niveau de /share se choisit ici. Pour ajouter un dossier, montez-le dans le compose, par exemple /mnt/nas/films:/share/films:ro."),
      h("div", { class: "actions" }, apply, rescan));
  };
  draw(share);
  state.refreshShare = async () => { if (card.isConnected) draw(await api("GET", "/share")); };
  return card;
}

// ---------- Événements en direct ----------

function debounce(fn, ms) {
  let t;
  return () => { clearTimeout(t); t = setTimeout(fn, ms); };
}

// Membres relus sans rien afficher, pour la barre d'état des autres vues.
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
    if (state.view === "telechargements" && state.refreshQueue) state.refreshQueue();
    else api("GET", "/queue?limit=1").then((d) => updateNavCount(d.summary)).catch(() => {});
  });
  es.addEventListener("members", debounce(() => {
    if (state.view === "membres" && state.refreshMembers) state.refreshMembers().catch(() => {});
    else refreshMembersQuietly();
  }, 500));
  es.addEventListener("settings", () => loadStatus().catch(() => {}));
  es.addEventListener("share", () => { if (state.view === "reglages") state.refreshShare?.().catch(() => {}); });
}

// ---------- Démarrage ----------

// Raccourcis clavier : 1, 2, 3 pour les vues, ? pour l'aide.
document.addEventListener("keydown", (ev) => {
  if (ev.ctrlKey || ev.metaKey || ev.altKey || document.body.classList.contains("login-mode")) return;
  if (["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName)) return;
  const views = { "1": "#/membres", "2": "#/telechargements", "3": "#/reglages" };
  if (views[ev.key] && state.status?.in_circle) { location.hash = views[ev.key]; ev.preventDefault(); }
  if (ev.key === "?") toast("raccourcis : 1 membres · 2 téléchargements · 3 réglages");
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
    if (e.login) return; // la page de connexion est affichée
    setView(h("div", { class: "card" }, h("h3", {}, "Interface indisponible"), h("p", { class: "err" }, e.message)));
  }
}

boot();
