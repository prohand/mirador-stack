// SPDX-License-Identifier: GPL-3.0-or-later
// mirador : petit script sans dépendance (CSP stricte : aucun script en ligne).
//  - data-confirmer="…" sur un bouton : demande confirmation avant d'envoyer ;
//  - data-rafraichir="URL" sur un élément : son contenu HTML est rechargé
//    toutes les data-intervalle secondes (défaut 30), en pause quand
//    l'onglet est caché (aucune requête inutile) ;
//  - graphiques : voir graphes.js.
"use strict";

document.addEventListener("click", function (e) {
  var b = e.target.closest("[data-confirmer]");
  if (b && !window.confirm(b.getAttribute("data-confirmer"))) {
    e.preventDefault();
  }
});

// Menu latéral : bouton replier / déplier. Sur grand écran, le choix est
// retenu dans le navigateur (localStorage) et l'emporte sur le réglage
// « menu replié par défaut » ; sur téléphone, le bouton ouvre ou ferme le
// menu (déroulant en haut).
(function () {
  var appli = document.getElementById("appli");
  var bouton = document.querySelector("[data-replier]");
  if (!appli || !bouton) { return; }
  var petit = window.matchMedia ? window.matchMedia("(max-width: 899px)") : { matches: false };
  var cle = "mirador.menu.replie";
  try {
    var v = window.localStorage.getItem(cle);
    if (v === "1") { appli.classList.add("replie"); }
    if (v === "0") { appli.classList.remove("replie"); }
  } catch (e) { /* stockage indisponible : réglage du serveur */ }
  bouton.addEventListener("click", function () {
    if (petit.matches) {
      appli.classList.toggle("ouvert");
      bouton.setAttribute("aria-expanded", appli.classList.contains("ouvert") ? "true" : "false");
      return;
    }
    var replie = appli.classList.toggle("replie");
    try { window.localStorage.setItem(cle, replie ? "1" : "0"); } catch (e) { /* rien */ }
  });
})();

(function () {
  var zones = [];
  function recharger(z) {
    if (document.hidden || z.enCours) { return; }
    // Pas de rechargement pendant une saisie ou un menu ouvert dans la zone
    // (filtre et colonnes de la vue d'ensemble) : il reviendra au tour suivant.
    var actif = document.activeElement;
    if ((actif && z.el.contains(actif) && actif.matches("input, select, textarea")) || z.el.querySelector("details.colonnes[open]")) { return; }
    z.enCours = true;
    fetch(z.url, { headers: { "X-Requested-With": "mirador" }, credentials: "same-origin" })
      .then(function (r) {
        if (r.status === 401) { window.location = "/connexion"; return null; }
        return r.ok ? r.text() : null;
      })
      .then(function (html) {
        // Le fragment vient du serveur (échappé par html/template) ; la CSP
        // interdit de toute façon les scripts qu'il pourrait contenir.
        if (html !== null) { z.el.innerHTML = html; document.dispatchEvent(new Event("mirador:fragment")); }
      })
      .catch(function () {})
      .finally(function () { z.enCours = false; });
  }
  document.querySelectorAll("[data-rafraichir]").forEach(function (el) {
    var z = { el: el, url: el.getAttribute("data-rafraichir"), enCours: false };
    var s = parseInt(el.getAttribute("data-intervalle") || "30", 10);
    if (s < 10) { s = 10; }
    zones.push(z);
    setInterval(function () { recharger(z); }, s * 1000);
  });
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) { zones.forEach(recharger); }
  });
})();

// Liste qui s'envoie seule quand on la change (data-envoyer) : largeur d'un
// panneau dans l'éditeur de tableaux.
document.addEventListener("change", function (e) {
  var s = e.target.closest("[data-envoyer]");
  if (s && s.form) { if (s.form.requestSubmit) { s.form.requestSubmit(); } else { s.form.submit(); } }
});

// Éditeur de tableaux : glisser-déposer des panneaux (d'une rangée à
// l'autre) et des rangées (par leur poignée). Au lâcher, le nouvel ordre est
// envoyé par un formulaire ordinaire (jeton CSRF compris) : le serveur
// vérifie que chaque rangée et chaque panneau y sont une fois et une seule.
(function () {
  var ed = document.querySelector("[data-editeur]");
  var form = document.getElementById("form-ordre");
  if (!ed || !form) { return; }
  var glisse = null, type = "", bouge = false;
  ed.addEventListener("dragstart", function (e) {
    var el = e.target.closest ? e.target.closest("[data-glisser]") : null;
    if (!el) { return; }
    type = el.getAttribute("data-glisser");
    glisse = type === "rangee" ? el.closest(".ed-rangee") : el;
    glisse.classList.add("glisse");
    bouge = false;
    e.dataTransfer.effectAllowed = "move";
    try { e.dataTransfer.setData("text/plain", "mirador"); } catch (x) { /* rien */ }
  });
  ed.addEventListener("dragover", function (e) {
    if (!glisse) { return; }
    e.preventDefault();
    if (type === "panneau") {
      var zone = e.target.closest("[data-zone]");
      if (!zone) { return; }
      var cible = e.target.closest("[data-glisser=panneau]");
      if (cible && cible !== glisse) {
        var b = cible.getBoundingClientRect();
        zone.insertBefore(glisse, e.clientX < b.left + b.width / 2 ? cible : cible.nextSibling);
        bouge = true;
      } else if (!cible && glisse.parentNode !== zone) {
        var vide = zone.querySelector(".vide");
        if (vide) { vide.remove(); }
        zone.appendChild(glisse);
        bouge = true;
      }
    } else {
      var r = e.target.closest(".ed-rangee");
      if (!r || r === glisse) { return; }
      var br = r.getBoundingClientRect();
      ed.insertBefore(glisse, e.clientY < br.top + br.height / 2 ? r : r.nextSibling);
      bouge = true;
    }
  });
  function finir() {
    if (!glisse) { return; }
    glisse.classList.remove("glisse");
    glisse = null;
    if (!bouge) { return; }
    var blocs = [];
    ed.querySelectorAll(".ed-rangee").forEach(function (r) {
      var ids = [];
      r.querySelectorAll("[data-glisser=panneau]").forEach(function (p) { ids.push(p.getAttribute("data-id")); });
      blocs.push(r.getAttribute("data-rangee") + ":" + ids.join(","));
    });
    form.elements.ordre.value = blocs.join("|");
    form.submit();
  }
  ed.addEventListener("drop", function (e) { e.preventDefault(); finir(); });
  ed.addEventListener("dragend", finir);
})();

// Éditeur de panneau : vérification en direct des requêtes (erreur en
// français, séries, exemple, explication, étiquettes cliquables pour la
// légende, unité proposée) et assistant « sans PromQL ». Tout ce qui vient
// du serveur est inséré comme TEXTE (textContent), jamais comme HTML.
(function () {
  var csrf = document.body.getAttribute("data-csrf") || "";
  function poster(url, donnees) {
    return fetch(url, { method: "POST", body: new URLSearchParams(donnees), credentials: "same-origin",
      headers: { "X-CSRF": csrf, "X-Requested-With": "mirador" } })
      .then(function (r) { return r.json(); });
  }
  function el(tag, classe, texte) {
    var e = document.createElement(tag);
    if (classe) { e.className = classe; }
    if (texte) { e.textContent = texte; }
    return e;
  }
  function remplir(select, valeurs, vide) {
    select.textContent = "";
    var o = el("option", "", vide); o.value = ""; select.appendChild(o);
    valeurs.forEach(function (v) { var x = el("option", "", v); x.value = v; select.appendChild(x); });
  }
  var minuteurs = {};
  function verifier(ta) {
    var i = ta.getAttribute("data-ligne");
    var aide = document.querySelector('[data-aide="' + i + '"]');
    if (!aide) { return; }
    var expr = ta.value.trim();
    if (!expr) { aide.textContent = ""; return; }
    var champUnite = ta.form && ta.form.elements.unite;
    poster(ta.getAttribute("data-verifier"), { expr: expr, unite: champUnite ? champUnite.value : "" }).then(function (r) {
      aide.textContent = "";
      if (!r.ok) {
        aide.appendChild(el("p", "erreur-texte", "✕ " + (r.erreur || "requête refusée")));
        if (r.explication) { aide.appendChild(el("p", "explication", r.explication)); }
        return;
      }
      aide.appendChild(el("p", "ok-texte", "✓ " + r.series + " série(s)" + (r.exemple ? " · exemple : " + r.exemple : "")));
      if (r.explication) { aide.appendChild(el("p", "explication", r.explication)); }
      if (r.etiquettes && r.etiquettes.length) {
        var p = el("p", "etiquettes", "Légende : ");
        var leg = ta.form.elements["legende" + i];
        r.etiquettes.forEach(function (e) {
          var b = el("button", "puce-etiquette", e.nom);
          b.type = "button";
          b.title = "Ajouter {{" + e.nom + "}} à la légende (ex. " + e.valeurs.join(", ") + ")";
          b.addEventListener("click", function () {
            if (leg) { leg.value = (leg.value ? leg.value + " " : "") + "{{" + e.nom + "}}"; }
          });
          p.appendChild(b);
        });
        aide.appendChild(p);
      }
      if (r.unite && champUnite && champUnite.value !== r.unite) {
        var u = el("p", "unite-proposee", "Unité proposée : " + (r.unite_nom || r.unite) + " ");
        var bu = el("button", "secondaire petit", "Appliquer");
        bu.type = "button";
        bu.addEventListener("click", function () { champUnite.value = r.unite; verifier(ta); });
        u.appendChild(bu);
        aide.appendChild(u);
      }
    }).catch(function () { aide.textContent = "Vérification impossible (connexion ?)"; });
  }
  document.querySelectorAll("textarea[data-verifier]").forEach(function (ta) {
    var i = ta.getAttribute("data-ligne");
    ta.addEventListener("input", function () {
      clearTimeout(minuteurs[i]);
      minuteurs[i] = setTimeout(function () { verifier(ta); }, 700);
    });
    if (ta.value.trim()) { verifier(ta); }
  });

  var a = document.querySelector("[data-assistant]");
  if (!a) { return; }
  function q(n) { return a.querySelector('[data-a="' + n + '"]'); }
  var base = "/tableaux/" + encodeURIComponent(a.getAttribute("data-tableau"));
  var etiquettes = [];
  q("metrique").addEventListener("change", function () {
    var opt = this.options[this.selectedIndex];
    q("calcul").value = opt && opt.getAttribute("data-compteur") ? "debit" : "valeur";
    remplir(q("par"), [], "(rien : une seule valeur)");
    remplir(q("filtre_etiquette"), [], "(aucun)");
    remplir(q("filtre_valeur"), [], "—");
    if (!this.value) { return; }
    fetch(base + "/etiquettes?metrique=" + encodeURIComponent(this.value), { credentials: "same-origin" })
      .then(function (r) { return r.json(); })
      .then(function (d) {
        etiquettes = d.etiquettes || [];
        var noms = etiquettes.map(function (e) { return e.nom; });
        remplir(q("par"), noms, "(rien : une seule valeur)");
        remplir(q("filtre_etiquette"), noms, "(aucun)");
        if (noms.indexOf("instance") >= 0) { q("par").value = "instance"; }
      }).catch(function () {});
  });
  q("filtre_etiquette").addEventListener("change", function () {
    var nom = this.value, e = null;
    etiquettes.forEach(function (x) { if (x.nom === nom) { e = x; } });
    remplir(q("filtre_valeur"), e ? e.valeurs : [], "—");
  });
  q("generer").addEventListener("click", function () {
    var d = { metrique: q("metrique").value, calcul: q("calcul").value, combiner: q("combiner").value, par: q("par").value,
      filtre_etiquette: q("filtre_etiquette").value, filtre_valeur: q("filtre_valeur").value };
    if (q("filtres_tableau").checked) { d.filtres_tableau = "1"; }
    poster(base + "/construire", d).then(function (r) {
      var err = q("erreur");
      if (r.erreur) { err.textContent = r.erreur; err.hidden = false; return; }
      err.hidden = true;
      var zones = document.querySelectorAll("textarea[data-verifier]"), ta = null;
      zones.forEach(function (z) { if (!ta && !z.value.trim()) { ta = z; } });
      if (!ta) { ta = zones[zones.length - 1]; }
      ta.value = r.expr;
      var leg = ta.form.elements["legende" + ta.getAttribute("data-ligne")];
      if (leg && r.legende) { leg.value = r.legende; }
      var u = ta.form.elements.unite;
      if (u && r.unite !== undefined) { u.value = r.unite || ""; }
      verifier(ta);
      ta.focus();
    }).catch(function () {});
  });
})();

// Vue d'ensemble : tri par colonne, filtre et colonnes masquées du tableau
// des machines. Les choix sont retenus dans le navigateur et ré-appliqués
// après chaque rechargement du tableau (toutes les minutes).
(function () {
  var cleTri = "mirador.parc.tri", cleCol = "mirador.parc.colonnes", cleEtat = "mirador.parc.etat";
  var filtre = "";
  function lire(cle, defaut) {
    try { var v = window.localStorage.getItem(cle); return v === null ? defaut : v; } catch (e) { return defaut; }
  }
  function ecrire(cle, v) { try { window.localStorage.setItem(cle, v); } catch (e) { /* rien */ } }
  function appliquer() {
    var zone = document.querySelector("[data-parc]");
    if (!zone) { return; }
    var table = zone.querySelector("table.parc");
    if (!table) { return; }
    // Titres triables : un vrai bouton dans chaque titre (clavier compris).
    table.querySelectorAll("th[data-tri]").forEach(function (th) {
      if (th.querySelector("button")) { return; }
      var b = document.createElement("button");
      b.type = "button";
      b.textContent = th.textContent;
      th.textContent = "";
      th.appendChild(b);
    });
    // Tri : « colonne:sens » ; vide = ordre du serveur (les plus en difficulté d'abord).
    var tri = lire(cleTri, "").split(":"), col = tri[0], sens = tri[1] === "asc" ? 1 : -1;
    var corps = table.tBodies[0], lignes = Array.prototype.slice.call(corps.rows);
    lignes.sort(function (a, b) {
      if (col === "nom") { return sens * a.getAttribute("data-nom").localeCompare(b.getAttribute("data-nom")); }
      if (col) {
        var x = parseFloat(a.getAttribute("data-" + col)), y = parseFloat(b.getAttribute("data-" + col));
        if (x !== y) { return sens * (x - y); }
      }
      return parseInt(a.getAttribute("data-rang"), 10) - parseInt(b.getAttribute("data-rang"), 10);
    });
    lignes.forEach(function (l) { corps.appendChild(l); });
    table.querySelectorAll("th[data-tri]").forEach(function (th) {
      if (th.getAttribute("data-tri") === col) { th.setAttribute("aria-sort", sens === 1 ? "ascending" : "descending"); }
      else { th.removeAttribute("aria-sort"); }
    });
    // Filtre : tous les mots tapés doivent se trouver dans nom, site, type ou groupe.
    var champ = zone.querySelector("[data-filtre-parc]");
    if (champ && champ.value !== filtre && document.activeElement !== champ) { champ.value = filtre; }
    var mots = filtre.toLowerCase().split(/\s+/).filter(Boolean), visibles = 0;
    // Filtre d'état : « probleme » (gravité ≥ 1), une gravité précise, ou
    // les machines qui ont des éléments à valider.
    var etat = lire(cleEtat, ""), choixEtat = zone.querySelector("[data-filtre-etat]");
    if (choixEtat && choixEtat.value !== etat) { choixEtat.value = etat; }
    function etatGarde(l) {
      var g = l.getAttribute("data-etat");
      if (etat === "") { return true; }
      if (etat === "probleme") { return parseInt(g, 10) >= 1; }
      if (etat === "valider") { return parseInt(l.getAttribute("data-valider"), 10) > 0; }
      if (etat === "majs") { return parseInt(l.getAttribute("data-majs"), 10) > 0; }
      return g === etat;
    }
    lignes.forEach(function (l) {
      var texte = l.getAttribute("data-texte").toLowerCase();
      var garde = etatGarde(l) && mots.every(function (m) { return texte.indexOf(m) >= 0; });
      l.hidden = !garde;
      if (garde) { visibles++; }
    });
    var vide = zone.querySelector("[data-parc-vide]");
    if (vide) { vide.hidden = visibles > 0; }
    // Colonnes masquées.
    var masquees = lire(cleCol, "").split(",").filter(Boolean);
    zone.querySelectorAll("[data-colonne]").forEach(function (c) {
      var nom = c.getAttribute("data-colonne"), masquee = masquees.indexOf(nom) >= 0;
      c.checked = !masquee;
      table.classList.toggle("sans-" + nom, masquee);
    });
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest("th[data-tri] button");
    if (!b) { return; }
    var col = b.parentNode.getAttribute("data-tri"), tri = lire(cleTri, "").split(":");
    // 1er clic : du plus grand au plus petit (noms : A → Z) ; 2e : l'inverse ; 3e : ordre du serveur.
    var premier = col === "nom" ? "asc" : "desc", second = col === "nom" ? "desc" : "asc";
    var suivant = tri[0] !== col ? col + ":" + premier : (tri[1] === premier ? col + ":" + second : "");
    ecrire(cleTri, suivant);
    appliquer();
  });
  document.addEventListener("input", function (e) {
    if (e.target.matches && e.target.matches("[data-filtre-parc]")) { filtre = e.target.value; appliquer(); }
  });
  document.addEventListener("change", function (e) {
    if (e.target.matches && e.target.matches("[data-filtre-etat]")) { ecrire(cleEtat, e.target.value); appliquer(); return; }
    if (!e.target.matches || !e.target.matches("[data-colonne]")) { return; }
    var masquees = [];
    document.querySelectorAll("[data-colonne]").forEach(function (c) { if (!c.checked) { masquees.push(c.getAttribute("data-colonne")); } });
    ecrire(cleCol, masquees.join(","));
    appliquer();
  });
  document.addEventListener("mirador:fragment", appliquer);
  appliquer();
})();

// Recherche rapide (Ctrl+K ou bouton « Aller à… ») : machines, tableaux et
// pages. La liste est lue une fois (/donnees/aller) ; le filtrage se fait
// ici. Tout est inséré comme TEXTE. Flèches et Entrée pour choisir.
(function () {
  var boite = null, champ = null, liste = null, tout = null, choix = 0;
  function construire() {
    boite = document.createElement("dialog");
    boite.className = "aller-boite";
    champ = document.createElement("input");
    champ.type = "search";
    champ.placeholder = "Machine, tableau ou page…";
    champ.setAttribute("aria-label", "Aller à");
    champ.maxLength = 63;
    liste = document.createElement("ul");
    boite.appendChild(champ);
    boite.appendChild(liste);
    document.body.appendChild(boite);
    champ.addEventListener("input", function () { choix = 0; afficher(); });
    champ.addEventListener("keydown", function (e) {
      var n = liste.children.length;
      if (e.key === "ArrowDown" && n) { choix = (choix + 1) % n; marquer(); e.preventDefault(); }
      if (e.key === "ArrowUp" && n) { choix = (choix + n - 1) % n; marquer(); e.preventDefault(); }
      if (e.key === "Enter" && liste.children[choix]) { window.location = liste.children[choix].firstChild.getAttribute("href"); }
    });
    boite.addEventListener("click", function (e) { if (e.target === boite) { boite.close(); } });
  }
  function marquer() {
    Array.prototype.forEach.call(liste.children, function (li, i) { li.classList.toggle("choisi", i === choix); });
    if (liste.children[choix]) { liste.children[choix].scrollIntoView({ block: "nearest" }); }
  }
  function afficher() {
    var mots = champ.value.toLowerCase().split(/\s+/).filter(Boolean);
    liste.textContent = "";
    (tout || []).filter(function (d) {
      var t = (d.titre + " " + d.genre).toLowerCase();
      return mots.every(function (m) { return t.indexOf(m) >= 0; });
    }).slice(0, 12).forEach(function (d) {
      var li = document.createElement("li"), a = document.createElement("a"), g = document.createElement("span");
      a.setAttribute("href", d.lien);
      a.textContent = d.titre;
      g.textContent = d.genre;
      a.appendChild(g);
      li.appendChild(a);
      liste.appendChild(li);
    });
    if (!liste.children.length) {
      var vide = document.createElement("li");
      vide.className = "vide";
      vide.textContent = tout ? "Rien ne correspond." : "Chargement…";
      liste.appendChild(vide);
    }
    marquer();
  }
  function ouvrir() {
    if (!window.HTMLDialogElement) { return; }
    if (!boite) { construire(); }
    if (boite.open) { return; }
    champ.value = "";
    choix = 0;
    boite.showModal();
    afficher();
    if (tout) { return; }
    fetch("/donnees/aller", { headers: { "X-Requested-With": "mirador" }, credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : []; })
      .then(function (d) {
        // Liens internes seulement (jamais une adresse complète).
        tout = (d || []).filter(function (x) { return typeof x.lien === "string" && x.lien.charAt(0) === "/" && x.lien.charAt(1) !== "/"; });
        afficher();
      })
      .catch(function () { tout = []; afficher(); });
  }
  document.addEventListener("keydown", function (e) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && !e.shiftKey && (e.key === "k" || e.key === "K")) { e.preventDefault(); ouvrir(); }
  });
  document.addEventListener("click", function (e) {
    if (e.target.closest && e.target.closest("[data-aller]")) { ouvrir(); }
  });
})();

// Code 2FA des actions sensibles : comme sur la page de connexion, un champ
// caché « nom d'utilisateur » rattache le formulaire au bon compte, pour que
// Vaultwarden / Bitwarden propose d'y remplir le code TOTP. Champ SANS nom :
// il n'est jamais envoyé au serveur.
(function () {
  var nom = document.body.getAttribute("data-utilisateur");
  if (!nom) { return; }
  var champs = document.querySelectorAll('form input[name="code"][autocomplete="one-time-code"]');
  for (var i = 0; i < champs.length; i++) {
    var f = champs[i].form;
    if (!f || f.querySelector('input[autocomplete="username"]')) { continue; }
    var u = document.createElement("input");
    u.type = "text";
    u.autocomplete = "username";
    u.value = nom;
    u.readOnly = true;
    u.hidden = true;
    u.tabIndex = -1;
    f.insertBefore(u, f.firstChild);
  }
})();

// Tableaux « Alertes en cours » (vue d'ensemble, fiche machine, Alertes) :
// tri par colonne (clic sur le titre) et filtres gravité + texte, retenus
// dans le navigateur. Sans JavaScript, le tableau reste dans l'ordre du
// serveur.
(function () {
  var cleTri = "mirador.alertes.tri", cleGravite = "mirador.alertes.gravite";
  function lire(cle, defaut) {
    try { var v = window.localStorage.getItem(cle); return v === null ? defaut : v; } catch (e) { return defaut; }
  }
  function ecrire(cle, v) { try { window.localStorage.setItem(cle, v); } catch (e) { /* rien */ } }
  var textes = {};
  function appliquer(zone, n) {
    var table = zone.querySelector("table");
    if (!table) { return; }
    table.querySelectorAll("th[data-tri]").forEach(function (th) {
      if (th.querySelector("button")) { return; }
      var b = document.createElement("button");
      b.type = "button";
      b.textContent = th.textContent;
      th.textContent = "";
      th.appendChild(b);
    });
    var tri = lire(cleTri, "").split(":"), col = tri[0], sens = tri[1] === "asc" ? 1 : -1;
    var corps = table.tBodies[0], lignes = Array.prototype.slice.call(corps.rows);
    lignes.sort(function (a, b) {
      if (col === "regle" || col === "machine") {
        var c = a.getAttribute("data-" + col).localeCompare(b.getAttribute("data-" + col));
        if (c !== 0) { return sens * c; }
      } else if (col) {
        var x = parseFloat(a.getAttribute("data-" + col)), y = parseFloat(b.getAttribute("data-" + col));
        if (x !== y) { return sens * (x - y); }
      }
      return parseInt(a.getAttribute("data-rang"), 10) - parseInt(b.getAttribute("data-rang"), 10);
    });
    lignes.forEach(function (l) { corps.appendChild(l); });
    table.querySelectorAll("th[data-tri]").forEach(function (th) {
      if (th.getAttribute("data-tri") === col) { th.setAttribute("aria-sort", sens === 1 ? "ascending" : "descending"); }
      else { th.removeAttribute("aria-sort"); }
    });
    var gravite = lire(cleGravite, ""), choix = zone.querySelector("[data-filtre-gravite]");
    if (choix && choix.value !== gravite) { choix.value = gravite; }
    var mots = (textes[n] || "").toLowerCase().split(/\s+/).filter(Boolean), visibles = 0;
    lignes.forEach(function (l) {
      var texte = l.getAttribute("data-texte").toLowerCase();
      var garde = (gravite === "" || l.getAttribute("data-gravite") === gravite) &&
        mots.every(function (m) { return texte.indexOf(m) >= 0; });
      l.hidden = !garde;
      if (garde) { visibles++; }
    });
    var vide = zone.querySelector("[data-alertes-vide]");
    if (vide) { vide.hidden = visibles > 0; }
  }
  function tout() {
    document.querySelectorAll("[data-alertes]").forEach(function (z, n) { appliquer(z, n); });
  }
  tout();
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-alertes] th[data-tri] button");
    if (!b) { return; }
    var col = b.parentNode.getAttribute("data-tri"), tri = lire(cleTri, "").split(":");
    // Premier clic : décroissant (la plus grave, la plus récente d'abord) ;
    // pour les noms : croissant. Troisième clic : ordre du serveur.
    var premier = (col === "regle" || col === "machine") ? "asc" : "desc";
    var second = premier === "asc" ? "desc" : "asc", nouveau;
    if (tri[0] !== col) { nouveau = col + ":" + premier; }
    else if (tri[1] === premier) { nouveau = col + ":" + second; }
    else { nouveau = ""; }
    ecrire(cleTri, nouveau);
    tout();
  });
  document.addEventListener("change", function (e) {
    if (e.target.matches && e.target.matches("[data-alertes] [data-filtre-gravite]")) {
      ecrire(cleGravite, e.target.value);
      tout();
    }
  });
  document.addEventListener("input", function (e) {
    if (e.target.matches && e.target.matches("[data-alertes] [data-filtre-alertes]")) {
      var zones = document.querySelectorAll("[data-alertes]");
      for (var n = 0; n < zones.length; n++) {
        if (zones[n].contains(e.target)) { textes[n] = e.target.value; appliquer(zones[n], n); }
      }
    }
  });
})();
