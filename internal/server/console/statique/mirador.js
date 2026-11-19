// SPDX-License-Identifier: GPL-3.0-or-later
// mirador : petit script sans dépendance (CSP stricte : aucun script en ligne).
//  - data-confirmer="…" sur un bouton : demande confirmation avant d'envoyer ;
//  - data-rafraichir="URL" sur un élément : son contenu HTML est rechargé
//    toutes les data-intervalle secondes (défaut 30), en pause quand
//    l'onglet est caché (aucune requête inutile) ;
//  - data-choix-fournisseur : montre les champs du fournisseur DNS choisi ;
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
    var table = zone && zone.querySelector("table.parc");
    if (!table) { chaleur([], false); return; }
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
    chaleur(lignes, etat !== "" || mots.length > 0);
    // Colonnes masquées.
    var masquees = lire(cleCol, "").split(",").filter(Boolean);
    zone.querySelectorAll("[data-colonne]").forEach(function (c) {
      var nom = c.getAttribute("data-colonne"), masquee = masquees.indexOf(nom) >= 0;
      c.checked = !masquee;
      table.classList.toggle("sans-" + nom, masquee);
    });
  }
  // Carte « Le parc sur 24 h » (SPEC § 37) : suit le filtre du tableau ;
  // les 10 plus chargées d'abord, le reste derrière « Voir les N autres ».
  var chaleurOuverte = {};
  function chaleur(lignesParc, filtrePose) {
    var cache = {};
    lignesParc.forEach(function (l) { cache[l.getAttribute("data-nom")] = l.hidden; });
    var total = 0;
    document.querySelectorAll("[data-chaleur]").forEach(function (bloc, n) {
      var gardees = 0, montrees = 0;
      bloc.querySelectorAll("tr[data-nom]").forEach(function (tr) {
        var nom = tr.getAttribute("data-nom");
        // Machine absente du tableau du parc : montrée seulement sans filtre.
        var garde = nom in cache ? !cache[nom] : !filtrePose;
        if (garde) { gardees++; }
        var montre = garde && (chaleurOuverte[n] || gardees <= 10);
        tr.hidden = !montre;
        if (montre) { montrees++; }
      });
      total += gardees;
      var b = bloc.querySelector("[data-chaleur-plus]");
      if (b) {
        b.hidden = gardees <= 10;
        b.textContent = chaleurOuverte[n] ? "Voir seulement les 10 plus chargées" : "Voir les " + (gardees - montrees) + " autres";
        b.setAttribute("data-n", n);
      }
    });
    var vide = document.querySelector("[data-chaleur-vide]");
    if (vide) { vide.hidden = total > 0 || !document.querySelector("[data-chaleur]"); }
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-chaleur-plus]");
    if (!b) { return; }
    var n = b.getAttribute("data-n");
    chaleurOuverte[n] = !chaleurOuverte[n];
    appliquer();
  });
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

// Recherche rapide (Ctrl+K ou bouton « Aller à… », SPEC § 58) : machines,
// conteneurs, sondes, services, tableaux, pages et actions. La liste est lue
// une fois (/donnees/aller) ; le filtrage se fait ici. Champ vide : favoris
// (☆ pour en ajouter) et pages récentes, retenus par ce navigateur. Tout est
// inséré comme TEXTE. Flèches et Entrée pour choisir.
(function () {
  var boite = null, champ = null, liste = null, tout = null, choix = 0;
  var cleRecents = "mirador-recents", cleFavoris = "mirador-favoris";
  function lire(cle) {
    try { var v = JSON.parse(window.localStorage.getItem(cle) || "[]"); return Array.isArray(v) ? v : []; } catch (e) { return []; }
  }
  function ecrire(cle, v) { try { window.localStorage.setItem(cle, JSON.stringify(v)); } catch (e) { /* stockage refusé */ } }
  function interne(l) { return typeof l === "string" && l.charAt(0) === "/" && l.charAt(1) !== "/"; }
  // Page récente : notée à chaque ouverture (8 au plus, sans doublon).
  (function () {
    var titre = document.title.replace(/ — mirador$/, ""), lien = window.location.pathname + window.location.search;
    if (/^\/(connexion|premier-admin|fragments|donnees)/.test(window.location.pathname)) { return; }
    var r = lire(cleRecents).filter(function (x) { return x && x.lien !== lien; });
    r.unshift({ titre: titre, lien: lien, genre: "Récent" });
    ecrire(cleRecents, r.slice(0, 8));
  })();
  function construire() {
    boite = document.createElement("dialog");
    boite.className = "aller-boite";
    champ = document.createElement("input");
    champ.type = "search";
    champ.placeholder = "Machine, conteneur, sonde, service, tableau, page…";
    champ.setAttribute("aria-label", "Aller à");
    champ.maxLength = 63;
    liste = document.createElement("ul");
    boite.appendChild(champ);
    boite.appendChild(liste);
    document.body.appendChild(boite);
    champ.addEventListener("input", function () { choix = 0; afficher(); });
    champ.addEventListener("keydown", function (e) {
      var liens = liste.querySelectorAll("a.aller-lien"), n = liens.length;
      if (e.key === "ArrowDown" && n) { choix = (choix + 1) % n; marquer(); e.preventDefault(); }
      if (e.key === "ArrowUp" && n) { choix = (choix + n - 1) % n; marquer(); e.preventDefault(); }
      if (e.key === "Enter" && liens[choix]) { window.location = liens[choix].getAttribute("href"); }
    });
    boite.addEventListener("click", function (e) { if (e.target === boite) { boite.close(); } });
  }
  function marquer() {
    var liens = liste.querySelectorAll("a.aller-lien");
    Array.prototype.forEach.call(liens, function (a, i) { a.parentNode.classList.toggle("choisi", i === choix); });
    if (liens[choix]) { liens[choix].scrollIntoView({ block: "nearest" }); }
  }
  function estFavori(lien) { return lire(cleFavoris).some(function (f) { return f.lien === lien; }); }
  function basculerFavori(d) {
    var f = lire(cleFavoris);
    if (estFavori(d.lien)) { f = f.filter(function (x) { return x.lien !== d.lien; }); } else { f.push({ titre: d.titre, lien: d.lien, genre: d.genre }); }
    ecrire(cleFavoris, f.slice(-20));
    afficher();
    champ.focus();
  }
  function section(titre) {
    var li = document.createElement("li");
    li.className = "aller-section";
    li.textContent = titre;
    liste.appendChild(li);
  }
  function ligne(d) {
    var li = document.createElement("li"), a = document.createElement("a"), g = document.createElement("span"), etoile = document.createElement("button");
    a.className = "aller-lien";
    a.setAttribute("href", d.lien);
    a.textContent = d.titre;
    g.textContent = d.genre;
    a.appendChild(g);
    etoile.type = "button";
    etoile.className = "aller-favori";
    var fav = estFavori(d.lien);
    etoile.textContent = fav ? "★" : "☆";
    etoile.title = fav ? "Retirer des favoris" : "Ajouter aux favoris";
    etoile.setAttribute("aria-label", etoile.title);
    etoile.addEventListener("click", function (e) { e.preventDefault(); basculerFavori(d); });
    li.appendChild(a);
    li.appendChild(etoile);
    liste.appendChild(li);
  }
  function afficher() {
    var mots = champ.value.toLowerCase().split(/\s+/).filter(Boolean);
    liste.textContent = "";
    if (!mots.length) {
      var fav = lire(cleFavoris).filter(function (x) { return x && interne(x.lien); });
      var rec = lire(cleRecents).filter(function (x) { return x && interne(x.lien) && !estFavori(x.lien); }).slice(0, 5);
      if (fav.length) { section("Favoris"); fav.forEach(ligne); }
      if (rec.length) { section("Récents"); rec.forEach(ligne); }
      if (tout) { section("Pages"); tout.filter(function (d) { return d.genre === "Page"; }).slice(0, 6).forEach(ligne); }
    } else {
      var trouves = (tout || []).filter(function (d) {
        var t = (d.titre + " " + d.genre).toLowerCase();
        return mots.every(function (m) { return t.indexOf(m) >= 0; });
      });
      var actions = trouves.filter(function (d) { return d.genre === "Action"; });
      trouves.filter(function (d) { return d.genre !== "Action"; }).slice(0, 12).forEach(ligne);
      if (actions.length) { section("Actions"); actions.slice(0, 4).forEach(ligne); }
    }
    if (!liste.querySelector("a.aller-lien")) {
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
        tout = (d || []).filter(function (x) { return interne(x.lien); });
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

// Tous les autres tableaux (demande du 12/11) : tri par clic sur un titre et,
// à partir de 6 lignes, un champ « Filtrer ce tableau ». Le tri comprend les
// nombres (« 1 234,5 »), les tailles (« 20,5 Go »), les durées (« il y a
// 3 min », « 2 h 05 ») et les dates (« 12/11/2026 08:14 »). Le tri choisi est
// retenu par page. Exclus : tableaux déjà triables (vue d'ensemble, alertes),
// tableaux d'édition (class « formulaire », l'ordre y compte), tableaux
// marqués data-sans-tri, lignes fusionnées (colspan).
(function () {
  var unites = { o: 1, ko: 1e3, mo: 1e6, go: 1e9, to: 1e12, kio: 1024, mio: 1048576, gio: 1073741824, tio: 1099511627776,
    ms: 0.001, s: 1, min: 60, h: 3600, j: 86400 };
  function valeur(td) {
    var brut = td.getAttribute("data-valeur");
    if (brut !== null) { var n = parseFloat(brut); return isNaN(n) ? brut.toLowerCase() : n; }
    var t = td.textContent.replace(/[  ]/g, " ").trim();
    if (t === "" || t === "—" || t === "-") { return null; }
    var d = t.match(/^(\d{2})\/(\d{2})\/(\d{4})(?:\s+(\d{2}):(\d{2})(?::(\d{2}))?)?/);
    if (d) { return Date.UTC(+d[3], +d[2] - 1, +d[1], +(d[4] || 0), +(d[5] || 0), +(d[6] || 0)); }
    var r = t.match(/^il y a\s+(\d+)\s*(s|min|h|j)\b/i);
    if (r) { return parseInt(r[1], 10) * unites[r[2].toLowerCase()]; }
    // Durées des règles : « 3d », « 15m », « 1h30m », « 0s ».
    if (/^(\d+(d|h|m|s))+$/.test(t)) {
      var total = 0, mult = { d: 86400, h: 3600, m: 60, s: 1 };
      t.replace(/(\d+)(d|h|m|s)/g, function (_, n, u) { total += parseInt(n, 10) * mult[u]; return ""; });
      return total;
    }
    var hm = t.match(/^(\d+)\s*h\s*(\d{2})$/);
    if (hm) { return parseInt(hm[1], 10) * 3600 + parseInt(hm[2], 10) * 60; }
    var m = t.match(/^(-?\d[\d ]*(?:[.,]\d+)?)\s*(%|[kKmMgGtT]?i?o|ms|s|min|h|j)?(?:\s|$|\/)/);
    if (m) {
      var x = parseFloat(m[1].replace(/ /g, "").replace(",", "."));
      if (!isNaN(x)) { return x * (m[2] && unites[m[2].toLowerCase()] ? unites[m[2].toLowerCase()] : 1); }
    }
    return t.toLowerCase();
  }
  function comparer(a, b) {
    if (a === null && b === null) { return 0; }
    if (a === null) { return 1; }
    if (b === null) { return -1; }
    if (typeof a === "number" && typeof b === "number") { return a - b; }
    return String(a).localeCompare(String(b), "fr", { numeric: true, sensitivity: "base" });
  }
  function lire(cle) { try { return window.localStorage.getItem(cle) || ""; } catch (e) { return ""; } }
  function ecrire(cle, v) { try { window.localStorage.setItem(cle, v); } catch (e) { /* rien */ } }
  function exclu(table) {
    return table.hasAttribute("data-sans-tri") || table.classList.contains("formulaire") || table.classList.contains("parc") ||
      table.closest("[data-alertes], [data-parc]") || table.querySelector("th[data-tri]") ||
      !table.tHead || !table.tBodies[0] || table.tBodies[0].rows.length < 2 || table.tBodies[0].querySelector("td[colspan], th[colspan]");
  }
  function trier(table, cle) {
    var etat = lire(cle).split(":"), col = etat[0] === "" ? -1 : parseInt(etat[0], 10), sens = etat[1] === "desc" ? -1 : 1;
    var corps = table.tBodies[0], lignes = Array.prototype.slice.call(corps.rows);
    lignes.sort(function (a, b) {
      if (col >= 0 && a.cells[col] && b.cells[col]) {
        var c = comparer(valeur(a.cells[col]), valeur(b.cells[col]));
        if (c !== 0) { return sens * c; }
      }
      return parseInt(a.getAttribute("data-rang-tri"), 10) - parseInt(b.getAttribute("data-rang-tri"), 10);
    });
    lignes.forEach(function (l) { corps.appendChild(l); });
    var ths = table.tHead.rows[0].cells;
    for (var i = 0; i < ths.length; i++) {
      if (i === col) { ths[i].setAttribute("aria-sort", sens === 1 ? "ascending" : "descending"); }
      else { ths[i].removeAttribute("aria-sort"); }
    }
  }
  function preparer(table, n) {
    if (table.getAttribute("data-trie") || exclu(table)) { return; }
    table.setAttribute("data-trie", "1");
    var cle = "mirador.tri." + location.pathname + "." + n;
    Array.prototype.forEach.call(table.tBodies[0].rows, function (l, i) { l.setAttribute("data-rang-tri", i); });
    var ths = table.tHead.rows[0].cells;
    Array.prototype.forEach.call(ths, function (th, i) {
      if (th.textContent.trim() === "" || th.querySelector("button, input, select, a")) { return; }
      th.setAttribute("data-tri", String(i)); // même style que les autres titres triables
      var b = document.createElement("button");
      b.type = "button";
      b.textContent = th.textContent.trim();
      th.textContent = "";
      th.appendChild(b);
      b.addEventListener("click", function () {
        // 1er clic : croissant ; 2e : décroissant ; 3e : ordre d'origine.
        var etat = lire(cle).split(":"), nouveau;
        if (etat[0] !== String(i)) { nouveau = i + ":asc"; }
        else if (etat[1] === "asc") { nouveau = i + ":desc"; }
        else { nouveau = ""; }
        ecrire(cle, nouveau);
        trier(table, cle);
      });
    });
    if (table.tBodies[0].rows.length >= 6 && !table.hasAttribute("data-sans-filtre")) {
      var champ = document.createElement("input");
      champ.type = "search";
      champ.placeholder = "Filtrer ce tableau…";
      champ.setAttribute("aria-label", "Filtrer ce tableau");
      champ.maxLength = 63;
      champ.className = "filtre-tableau";
      var avant = table.closest(".defile") || table;
      avant.parentNode.insertBefore(champ, avant);
      champ.addEventListener("input", function () {
        var mots = champ.value.toLowerCase().split(/\s+/).filter(Boolean);
        Array.prototype.forEach.call(table.tBodies[0].rows, function (l) {
          var texte = l.textContent.toLowerCase();
          l.hidden = !mots.every(function (m) { return texte.indexOf(m) >= 0; });
        });
      });
    }
    if (lire(cle)) { trier(table, cle); }
  }
  function tout() {
    var tables = document.querySelectorAll("table");
    var vues = [];
    Array.prototype.forEach.call(tables, function (t) {
      if (vues.indexOf(t) >= 0) { return; }
      vues.push(t);
      preparer(t, vues.length - 1);
    });
  }
  tout();
  document.addEventListener("mirador:fragment", tout);
})();

// --- Thème par utilisateur (SPEC § 25) ------------------------------------------
// Sélecteur Jour / Nuit / Auto en bas du menu : le thème change tout de
// suite, puis le choix est enregistré dans le compte (sans recharger).
// Menu replié : seul le choix actif est montré, un clic passe au suivant.
(function () {
  "use strict";
  var form = document.querySelector("[data-themes]");
  if (!form) { return; }
  var ordre = ["clair", "sombre", "auto"];
  var boutons = form.querySelectorAll("button[name=theme]");
  function appliquer(theme) {
    if (theme === "auto") { document.documentElement.removeAttribute("data-theme"); }
    else { document.documentElement.setAttribute("data-theme", theme); }
    boutons.forEach(function (b) { b.setAttribute("aria-checked", b.value === theme ? "true" : "false"); });
    document.dispatchEvent(new CustomEvent("mirador:theme", { detail: theme }));
  }
  form.addEventListener("click", function (e) {
    var b = e.target.closest("button[name=theme]");
    if (!b) { return; }
    e.preventDefault();
    var theme = b.value;
    var appli = document.getElementById("appli");
    if (appli && appli.classList.contains("replie") && b.getAttribute("aria-checked") === "true") {
      theme = ordre[(ordre.indexOf(theme) + 1) % ordre.length];
    }
    appliquer(theme);
    fetch(form.action, { method: "POST", credentials: "same-origin",
      headers: { "X-Requested-With": "mirador", "X-CSRF": document.body.getAttribute("data-csrf") || "" },
      body: new URLSearchParams({ theme: theme }) }).catch(function () { /* gardé pour cette page */ });
  });
})();

// Réglages du certificat : seuls les champs du fournisseur DNS choisi sont
// montrés et envoyés (les autres sont désactivés).
document.addEventListener("change", function (e) {
  var choix = e.target.closest("[data-choix-fournisseur]");
  if (!choix) { return; }
  choix.form.querySelectorAll("[data-fournisseur]").forEach(function (f) {
    var actif = f.getAttribute("data-fournisseur") === choix.value;
    f.hidden = !actif;
    f.disabled = !actif;
  });
});

// --- Aides affichées / masquées (SPEC § 26) ---------------------------------------
// Bouton « Avec aides / Sans aides » en bas du menu : la page change tout de
// suite, puis le choix est enregistré dans le compte (sans recharger).
// Menu replié : seul le choix actif est montré, un clic passe à l'autre.
(function () {
  "use strict";
  var form = document.querySelector("[data-aides]");
  if (!form) { return; }
  var boutons = form.querySelectorAll("button[name=aides]");
  form.addEventListener("click", function (e) {
    var b = e.target.closest("button[name=aides]");
    if (!b) { return; }
    e.preventDefault();
    var choix = b.value;
    var appli = document.getElementById("appli");
    if (appli && appli.classList.contains("replie") && b.getAttribute("aria-checked") === "true") {
      choix = choix === "avec" ? "sans" : "avec";
    }
    document.body.classList.toggle("sans-aides", choix === "sans");
    boutons.forEach(function (x) { x.setAttribute("aria-checked", x.value === choix ? "true" : "false"); });
    fetch(form.action, { method: "POST", credentials: "same-origin",
      headers: { "X-Requested-With": "mirador", "X-CSRF": document.body.getAttribute("data-csrf") || "" },
      body: new URLSearchParams({ aides: choix }) }).catch(function () { /* gardé pour cette page */ });
  });
})();

// --- Alertes (SPEC § 28) ------------------------------------------------------
// « Nouvelles depuis ta dernière visite » (heure de la visite précédente
// gardée dans ce navigateur), nombre d'alertes dans le titre de l'onglet,
// refait après chaque rechargement du bloc ; menu « Silence » refermé au
// clic ailleurs.
(function () {
  "use strict";
  if (!document.querySelector("[data-nb-alertes]")) { return; }
  var CLE = "mirador.alertes.vu", vu = 0, titre = document.title.replace(/^\(\d+\) /, "");
  try { vu = parseInt(window.localStorage.getItem(CLE) || "0", 10) || 0; } catch (e) { vu = 0; }
  function marquer() {
    var z = document.querySelector("[data-nb-alertes]");
    if (!z) { return; }
    var n = 0;
    z.querySelectorAll("tr[data-depuis]").forEach(function (tr) {
      var nouvelle = vu > 0 && parseInt(tr.getAttribute("data-depuis"), 10) > vu;
      tr.classList.toggle("nouvelle", nouvelle);
      if (nouvelle) { n++; }
    });
    var c = z.querySelector("[data-nb-nouvelles]");
    if (c) { c.textContent = String(n); }
    var t = parseInt(z.getAttribute("data-nb-alertes") || "0", 10);
    document.title = (t > 0 ? "(" + t + ") " : "") + titre;
  }
  marquer();
  document.addEventListener("mirador:fragment", marquer);
  function noter() { try { window.localStorage.setItem(CLE, String(Date.now())); } catch (e) { /* navigateur sans stockage */ } }
  window.addEventListener("pagehide", noter);
  document.addEventListener("visibilitychange", function () { if (document.hidden) { noter(); } });
  document.addEventListener("click", function (e) {
    document.querySelectorAll("details.menu-silence[open]").forEach(function (d) {
      if (!d.contains(e.target)) { d.removeAttribute("open"); }
    });
  });
})();

// --- Page d'une machine (SPEC § 29) -------------------------------------------
// Sections de graphiques repliées : retenues dans ce navigateur
// (data-memoire-replie = nom de la page).
(function () {
  "use strict";
  var zone = document.querySelector("[data-memoire-replie]");
  if (!zone) { return; }
  var cle = "mirador.replie." + zone.getAttribute("data-memoire-replie");
  var replies = {};
  var jamaisRegle = false;
  try {
    var brut = window.localStorage.getItem(cle);
    jamaisRegle = brut === null;
    replies = JSON.parse(brut || "{}") || {};
  } catch (e) { replies = {}; }
  // Téléphone, première visite : seule la première section est ouverte
  // (page courte, courbes des autres sections pas chargées). Ensuite, ce
  // que l'on ouvre ou replie est retenu comme sur grand écran.
  var court = jamaisRegle && window.matchMedia && window.matchMedia("(max-width: 640px)").matches;
  zone.querySelectorAll("details.rangee").forEach(function (d, i) {
    if (replies[d.id] || (court && i > 0)) { d.removeAttribute("open"); }
    d.addEventListener("toggle", function () {
      if (d.open) { delete replies[d.id]; } else { replies[d.id] = 1; }
      try { window.localStorage.setItem(cle, JSON.stringify(replies)); } catch (e) { /* navigateur sans stockage */ }
    });
  });
})();

// Rechargement automatique (SPEC § 32) : [data-recharger="15"] tant qu'une
// étape avance (création d'un certificat), sauf si on est en train de
// remplir un champ.
(function () {
  var el = document.querySelector("[data-recharger]");
  if (!el) { return; }
  var s = Math.max(5, parseInt(el.getAttribute("data-recharger"), 10) || 15);
  setInterval(function () {
    var a = document.activeElement;
    if (document.hidden || (a && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName))) { return; }
    window.location.reload();
  }, s * 1000);
})();

// Page Logs (SPEC § 33) : détail d'une ligne au clic, copier, zoom sur
// l'histogramme, mode direct. Le contenu des logs est toujours posé avec
// textContent (jamais comme du HTML).
(function () {
  function basculer(tr) {
    var d = tr.nextElementSibling;
    if (!d || !d.classList.contains("logs-detail")) { return; }
    d.hidden = !d.hidden;
    tr.setAttribute("aria-expanded", d.hidden ? "false" : "true");
    tr.classList.toggle("ouvert", !d.hidden);
  }
  document.addEventListener("click", function (e) {
    var tr = e.target.closest && e.target.closest("[data-logs-ouvrir]");
    if (tr && !e.target.closest("a, button") && !(window.getSelection && String(window.getSelection()))) { basculer(tr); }
  });
  document.addEventListener("keydown", function (e) {
    var tr = e.target.closest && e.target.closest("[data-logs-ouvrir]");
    if (tr && (e.key === "Enter" || e.key === " ")) { e.preventDefault(); basculer(tr); }
  });
  function copier(texte, bouton) {
    var fini = function () {
      var avant = bouton.textContent;
      bouton.textContent = "Copié ✓";
      setTimeout(function () { bouton.textContent = avant; }, 1500);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) { navigator.clipboard.writeText(texte).then(fini, function () {}); }
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-copier-ligne], [data-copier-json], [data-copier-lien]");
    if (!b) { return; }
    e.preventDefault();
    if (b.hasAttribute("data-copier-json")) { copier(b.getAttribute("data-copier-json"), b); return; }
    if (b.hasAttribute("data-copier-lien")) { copier(new URL(b.getAttribute("href"), window.location.href).href, b); return; }
    var tr = b.closest(".logs-detail").previousElementSibling;
    copier(Array.prototype.map.call(tr.cells, function (c) { return c.textContent.trim(); }).filter(Boolean).join(" "), b);
  });

  // Zoom : glisser sur l'histogramme choisit une période (de, a).
  var svg = document.querySelector("[data-zoom-logs]");
  if (svg) {
    var debut = parseInt(svg.getAttribute("data-debut"), 10), fin = parseInt(svg.getAttribute("data-fin"), 10);
    var sel = svg.querySelector(".h-selection"), x0 = null;
    var pos = function (e) {
      var r = svg.getBoundingClientRect();
      return Math.max(0, Math.min(1, (e.clientX - r.left) / r.width));
    };
    svg.addEventListener("mousedown", function (e) { x0 = pos(e); e.preventDefault(); });
    window.addEventListener("mousemove", function (e) {
      if (x0 === null) { return; }
      var x = pos(e);
      sel.setAttribute("x", String(Math.min(x0, x) * 1000));
      sel.setAttribute("width", String(Math.abs(x - x0) * 1000));
    });
    window.addEventListener("mouseup", function (e) {
      if (x0 === null) { return; }
      var x = pos(e), a = Math.min(x0, x), b = Math.max(x0, x);
      x0 = null;
      if (b - a < 0.01) { sel.setAttribute("width", "0"); return; }
      var q = new URLSearchParams(window.location.search);
      q.set("de", String(Math.round(debut + (fin - debut) * a)));
      q.set("a", String(Math.round(debut + (fin - debut) * b)));
      q.delete("avant");
      window.location = "/logs?" + q.toString();
    });
  }

  // Mode direct : nouvelles lignes toutes les 5 s, en haut du tableau.
  var bouton = document.querySelector("[data-logs-direct]");
  if (!bouton) { return; }
  var minuterie = null, finDirect = 0, dernier = 0, nouvelles = 0;
  var bandeau = document.querySelector("[data-logs-nouvelles]");
  document.querySelectorAll("[data-logs-ouvrir][data-t]").forEach(function (tr) { dernier = Math.max(dernier, parseInt(tr.getAttribute("data-t"), 10) || 0); });
  function enPause() {
    var t = document.querySelector("[data-logs-table]");
    return document.hidden || (t && t.matches(":hover")) || document.querySelector(".logs-detail:not([hidden])");
  }
  function cellule(tr, texte, classe) {
    var td = document.createElement("td");
    if (classe) { td.className = classe; }
    td.textContent = texte;
    tr.appendChild(td);
    return td;
  }
  function ajouter(lignes) {
    var t = document.querySelector("[data-logs-table]");
    if (!t || !lignes.length) { return; }
    t.hidden = false;
    var corps = t.tBodies[0], avant = corps.querySelector("tr.logs-ligne-log");
    // Plus anciennes d'abord dans la boucle : la plus récente finit en haut.
    lignes.slice().reverse().forEach(function (l) {
      var tr = document.createElement("tr");
      tr.className = "logs-ligne-log nouvelle " + (l.classe || "");
      cellule(tr, l.heure, "mono");
      cellule(tr, l.host);
      var niv = cellule(tr, "");
      if (l.niveau) { var s = document.createElement("span"); s.className = "logs-niv " + (l.classe || ""); s.textContent = l.niveau; niv.appendChild(s); }
      cellule(tr, l.origine || l.job);
      cellule(tr, l.msg, "logs-msg");
      corps.insertBefore(tr, avant);
      avant = tr;
      dernier = Math.max(dernier, l.t);
    });
    nouvelles += lignes.length;
    if (bandeau) { bandeau.hidden = false; bandeau.textContent = "↑ " + nouvelles + " nouvelle(s) ligne(s) depuis l'activation du mode direct"; }
  }
  function tirer() {
    if (Date.now() > finDirect) { arreter(); return; }
    if (enPause()) { return; }
    fetch(bouton.getAttribute("data-logs-direct") + "&depuis=" + dernier, { headers: { "X-Requested-With": "mirador" }, credentials: "same-origin" })
      .then(function (r) { if (r.status === 401) { window.location = "/connexion"; return null; } return r.json(); })
      .then(function (d) { if (d && d.lignes) { ajouter(d.lignes); } })
      .catch(function () {});
  }
  function arreter() {
    clearInterval(minuterie);
    minuterie = null;
    bouton.classList.remove("actif");
    bouton.setAttribute("aria-pressed", "false");
    bouton.querySelector("span").textContent = "Direct";
  }
  bouton.addEventListener("click", function () {
    if (minuterie) { arreter(); return; }
    if (!dernier) { dernier = Date.now() - 60000; }
    finDirect = Date.now() + 30 * 60 * 1000; // arrêt seul après 30 min
    bouton.classList.add("actif");
    bouton.setAttribute("aria-pressed", "true");
    bouton.querySelector("span").textContent = "En direct · 5 s (pause au survol)";
    tirer();
    minuterie = setInterval(tirer, 5000);
  });
})();

// Actions protégées (boutons data-code) : si le mode administration est
// fermé, le code 2FA est demandé dans une petite boîte au moment d'envoyer,
// sans perdre ce qui est saisi dans le formulaire. Le serveur vérifie tout ;
// sans JavaScript, il répond « code 2FA demandé » et le mode administration
// s'ouvre depuis « Mon compte ».
//  - data-code : toujours ;
//  - data-code-sauf="nom" : pas si la case de ce nom est cochée (essai) ;
//  - data-code-long="nom" : seulement si la durée du champ dépasse 24 h.
(function () {
  if (!window.HTMLDialogElement) { return; }
  var boite = null, champ = null, suite = null;
  // Durées du serveur : « 90m », « 2h », « 1d12h », « 1w »… en secondes
  // (NaN si le texte n'est pas une durée : le serveur tranchera).
  function secondes(texte) {
    var unites = { ms: 0.001, s: 1, m: 60, h: 3600, d: 86400, w: 604800, y: 31536000 };
    var re = /(\d+)(ms|s|m|h|d|w|y)/g, total = 0, lu = 0, m;
    texte = String(texte).trim();
    while ((m = re.exec(texte))) { total += parseInt(m[1], 10) * unites[m[2]]; lu += m[0].length; }
    return lu === texte.length && lu > 0 ? total : NaN;
  }
  function demander(apres) {
    if (!boite) {
      boite = document.createElement("dialog");
      boite.className = "code-boite";
      var f = document.createElement("form");
      f.method = "dialog";
      var titre = document.createElement("h2");
      titre.textContent = "Action protégée";
      var texte = document.createElement("p");
      texte.textContent = "Saisir le code 2FA. Il ouvre aussi le mode administration : plus de code pendant quelques minutes.";
      // Champ « nom d'utilisateur » sans nom : pour que le gestionnaire de
      // mots de passe propose le bon code (jamais envoyé au serveur).
      // Mêmes repères que la page de connexion (Bitwarden / Vaultwarden
      // cherchent « username » puis un champ « totp » / « one-time-code ») :
      // sans nom ni id, Bitwarden ne proposait pas le code ici (19/11).
      // Formulaire « dialog » : rien n'est envoyé au serveur par ce biais.
      var u = document.createElement("input");
      u.type = "text"; u.name = "username"; u.autocomplete = "username"; u.readOnly = true; u.hidden = true; u.tabIndex = -1;
      u.value = document.body.getAttribute("data-utilisateur") || "";
      var label = document.createElement("label");
      label.className = "code toujours";
      label.textContent = "Code 2FA ";
      champ = document.createElement("input");
      champ.id = "totp-boite"; champ.name = "totp";
      champ.setAttribute("aria-label", "Code 2FA à usage unique (TOTP)");
      champ.type = "text"; champ.inputMode = "numeric"; champ.autocomplete = "one-time-code";
      champ.pattern = "[0-9 ]{6,7}"; champ.maxLength = 7; champ.required = true; champ.placeholder = "123456";
      label.appendChild(champ);
      var boutons = document.createElement("div");
      boutons.className = "ligne";
      var non = document.createElement("button");
      non.type = "button"; non.className = "secondaire"; non.textContent = "Annuler";
      non.addEventListener("click", function () { suite = null; boite.close(); });
      var oui = document.createElement("button");
      oui.textContent = "Valider";
      boutons.appendChild(non); boutons.appendChild(oui);
      f.appendChild(titre); f.appendChild(texte); f.appendChild(u); f.appendChild(label); f.appendChild(boutons);
      f.addEventListener("submit", function () {
        var s = suite, code = champ.value;
        suite = null;
        if (s) { window.setTimeout(function () { s(code); }, 0); }
      });
      boite.appendChild(f);
      document.body.appendChild(boite);
    }
    suite = apres;
    champ.value = "";
    boite.showModal();
    champ.focus();
  }
  document.addEventListener("submit", function (e) {
    var b = e.submitter, form = e.target;
    if (!b || !form || !form.querySelector || document.body.hasAttribute("data-mode-admin")) { return; }
    var besoin = b.hasAttribute("data-code");
    if (besoin && b.getAttribute("data-code-sauf")) {
      var c = form.querySelector('input[type="checkbox"][name="' + b.getAttribute("data-code-sauf") + '"]');
      besoin = !(c && c.checked);
    }
    if (!besoin && b.getAttribute("data-code-long")) {
      var d = form.elements[b.getAttribute("data-code-long")];
      besoin = !!d && secondes(d.value) > 86400;
    }
    if (!besoin) { return; }
    var saisi = form.querySelector('input[name="code"]');
    if (saisi && saisi.value.trim() !== "") { return; } // code déjà là : on envoie
    e.preventDefault();
    demander(function (code) {
      if (!saisi) {
        saisi = document.createElement("input");
        saisi.type = "hidden"; saisi.name = "code";
        form.appendChild(saisi);
      }
      saisi.value = code;
      // Même bouton (son nom et sa valeur partent avec le formulaire).
      if (form.requestSubmit) { form.requestSubmit(b); } else { form.submit(); }
    });
  });
})();

// Téléphone (SPEC § 34.4) : bouton « Plus » de la barre du bas. Il ouvre ou
// ferme tout le menu, affiché en plein écran au-dessus de la barre.
(function () {
  var appli = document.getElementById("appli");
  var plus = document.querySelector("[data-menu-plus]");
  if (!appli || !plus) { return; }
  function regler(ouvert) {
    appli.classList.toggle("ouvert", ouvert);
    document.body.classList.toggle("menu-ouvert", ouvert);
    plus.setAttribute("aria-expanded", ouvert ? "true" : "false");
    plus.classList.toggle("actif", ouvert);
  }
  plus.addEventListener("click", function () { regler(!appli.classList.contains("ouvert")); });
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") { regler(false); } });
  // « Aller à… » depuis le menu ouvert : le menu se referme derrière la recherche.
  document.addEventListener("click", function (e) {
    if (e.target.closest && e.target.closest("[data-aller]")) { regler(false); }
  });
})();

// Téléphone : tableaux marqués data-cartes affichés en cartes (une par
// ligne), chaque valeur précédée du titre de sa colonne. Le titre est copié
// ici dans data-label ; sans JavaScript, le tableau reste un tableau que
// l'on fait défiler de côté.
(function () {
  function etiqueter() {
    document.querySelectorAll("table[data-cartes]").forEach(function (t) {
      var tete = t.tHead && t.tHead.rows[0];
      if (!tete) { return; }
      var noms = Array.prototype.map.call(tete.cells, function (th) { return th.textContent.trim(); });
      Array.prototype.forEach.call(t.tBodies, function (corps) {
        Array.prototype.forEach.call(corps.rows, function (ligne) {
          Array.prototype.forEach.call(ligne.cells, function (c, i) {
            if (!c.hasAttribute("data-label")) { c.setAttribute("data-label", noms[i] || ""); }
            // Texte long (phrase de l'audit, résumé…) : titre au-dessus,
            // texte sur toute la largeur, au lieu d'une colonne étroite.
            if (i > 0 && c.textContent.trim().length > 36) { c.setAttribute("data-long", ""); }
          });
        });
      });
      t.classList.add("en-cartes");
    });
  }
  etiqueter();
  document.addEventListener("mirador:fragment", etiqueter);
})();

// Bouton « Copier » (SPEC § 36) : [data-copier="#id"] copie le texte de
// l'élément visé (commande d'installation, token d'un équipement).
(function () {
  document.addEventListener("click", function (e) {
    var b = e.target.closest && e.target.closest("[data-copier]");
    if (!b) { return; }
    var cible = document.querySelector(b.getAttribute("data-copier"));
    if (!cible || !navigator.clipboard || !navigator.clipboard.writeText) { return; }
    e.preventDefault();
    navigator.clipboard.writeText(cible.textContent.trim()).then(function () {
      var avant = b.textContent;
      b.textContent = "Copié ✓";
      setTimeout(function () { b.textContent = avant; }, 1500);
    }, function () {});
  });
})();

// Barre « Enregistrer » collée en bas (SPEC § 38) : dit s'il y a des
// changements pas encore enregistrés dans le formulaire.
(function () {
  document.querySelectorAll("form[data-barre-enregistrer]").forEach(function (f) {
    var barre = f.querySelector(".barre-enregistrer"), etat = f.querySelector("[data-barre-etat]");
    if (!barre || !etat) { return; }
    var n = {};
    function noter(e) {
      var c = e.target;
      if (!c.name || c.name === "code" || c.name === "csrf") { return; }
      n[c.name] = true;
      var k = Object.keys(n).length;
      barre.classList.add("modifie");
      etat.textContent = k === 1 ? "1 changement non enregistré" : k + " changements non enregistrés";
    }
    f.addEventListener("input", noter);
    f.addEventListener("change", noter);
  });
})();

// Blocs repliables retenus (SPEC § 42) : <details data-memoire-ouvert="nom">.
// data-ouvrir : ouvert d'office (problème : service en échec, mises à jour
// de sécurité en attente) ; sinon l'état choisi est retenu par ce navigateur.
(function () {
  "use strict";
  document.querySelectorAll("details[data-memoire-ouvert]").forEach(function (d) {
    var cle = "mirador.ouvert." + d.getAttribute("data-memoire-ouvert");
    var ouvert = false;
    try { ouvert = window.localStorage.getItem(cle) === "1"; } catch (e) { /* navigateur sans stockage */ }
    if (d.hasAttribute("data-ouvrir") || ouvert) { d.open = true; }
    // L'écoute commence après l'ouverture d'office : seul un clic est retenu.
    window.setTimeout(function () {
      d.addEventListener("toggle", function () {
        try { window.localStorage.setItem(cle, d.open ? "1" : "0"); } catch (e) { /* rien */ }
      });
    }, 0);
  });
})();

// Onglets qui filtrent les lignes d'un tableau (SPEC § 42.2) :
// <p data-onglets-lignes="nom"><button data-filtre="echec">…</button></p> et
// <table data-lignes-filtrees="nom"> dont les lignes ont data-groupes="echec …".
// Le filtre « Filtrer ce tableau » reste indépendant (attribut à part).
(function () {
  "use strict";
  document.querySelectorAll("[data-onglets-lignes]").forEach(function (barre) {
    var table = document.querySelector('table[data-lignes-filtrees="' + barre.getAttribute("data-onglets-lignes") + '"]');
    if (!table) { return; }
    barre.addEventListener("click", function (e) {
      var b = e.target.closest("button[data-filtre]");
      if (!b) { return; }
      var f = b.getAttribute("data-filtre");
      barre.querySelectorAll("button[data-filtre]").forEach(function (x) {
        x.classList.toggle("actif", x === b);
        x.setAttribute("aria-pressed", x === b ? "true" : "false");
      });
      Array.prototype.forEach.call(table.tBodies[0].rows, function (l) {
        var g = " " + (l.getAttribute("data-groupes") || "") + " ";
        if (f === "" || g.indexOf(" " + f + " ") >= 0) { l.removeAttribute("data-onglet-cache"); }
        else { l.setAttribute("data-onglet-cache", ""); }
      });
    });
  });
})();

// Panneau de détail au clic (SPEC § 42.4) : a[data-panneau] charge un
// fragment du serveur dans un panneau à droite (au-dessus d'un voile).
// Fermeture : bouton, Échap, clic sur le voile ; le focus revient au lien.
// Sans JavaScript, le lien ouvre une page complète.
(function () {
  "use strict";
  var voile = null, panneau = null, origine = null;
  function creer() {
    if (panneau) { return; }
    voile = document.createElement("div");
    voile.className = "voile-panneau";
    voile.hidden = true;
    panneau = document.createElement("aside");
    panneau.className = "panneau-detail";
    panneau.setAttribute("role", "dialog");
    panneau.setAttribute("aria-modal", "true");
    panneau.setAttribute("aria-label", "Détail");
    panneau.hidden = true;
    panneau.tabIndex = -1;
    document.body.appendChild(voile);
    document.body.appendChild(panneau);
    voile.addEventListener("click", fermer);
  }
  function fermer() {
    if (!panneau || panneau.hidden) { return; }
    panneau.hidden = true;
    voile.hidden = true;
    document.body.classList.remove("panneau-ouvert");
    if (origine && origine.focus) { origine.focus(); }
  }
  function ouvrir(lien) {
    creer();
    if (panneau.hidden) { origine = lien; }
    panneau.setAttribute("aria-busy", "true");
    fetch(lien.getAttribute("href"), { headers: { "X-Requested-With": "mirador" }, credentials: "same-origin" })
      .then(function (r) { if (!r.ok) { throw new Error(String(r.status)); } return r.text(); })
      .then(function (html) {
        panneau.innerHTML = html; // fragment du serveur (gabarit échappé)
        panneau.hidden = false;
        voile.hidden = false;
        document.body.classList.add("panneau-ouvert");
        panneau.removeAttribute("aria-busy");
        var f = panneau.querySelector("[data-fermer-panneau]");
        (f || panneau).focus();
      })
      .catch(function () { window.location.href = lien.getAttribute("href"); });
  }
  document.addEventListener("click", function (e) {
    var a = e.target.closest && e.target.closest("a[data-panneau]");
    if (a && !e.ctrlKey && !e.metaKey && !e.shiftKey && e.button === 0) { e.preventDefault(); ouvrir(a); return; }
    if (e.target.closest && e.target.closest("[data-fermer-panneau]")) {
      if (panneau && panneau.contains(e.target)) { fermer(); }
      else { window.history.back(); }
    }
  });
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") { fermer(); } });
})();

// Vue d'ensemble : onglet Résumé / Ruche retenu par ce navigateur (SPEC § 51).
(function () {
  var cle = "mirador-vue-accueil";
  document.addEventListener("click", function (ev) {
    var a = ev.target.closest ? ev.target.closest("[data-vue-accueil]") : null;
    if (a) { try { window.localStorage.setItem(cle, a.getAttribute("data-vue-accueil")); } catch (e) { /* stockage refusé */ } }
  });
  if (window.location.pathname === "/" && !window.location.search) {
    try { if (window.localStorage.getItem(cle) === "ruche") { window.location.replace("/?vue=ruche"); } } catch (e) { /* stockage refusé */ }
  }
})();

// Frise d'un incident (SPEC § 54) : position des points et de la bande
// (data-x, data-l en %), posée par le script (pas de style dans la page).
(function () {
  document.querySelectorAll(".frise [data-x]").forEach(function (e) {
    var x = parseFloat(e.getAttribute("data-x"));
    if (isNaN(x)) { return; }
    e.style.left = Math.max(0, Math.min(100, x)) + "%";
    var l = parseFloat(e.getAttribute("data-l"));
    if (!isNaN(l)) { e.style.width = Math.max(0.5, Math.min(100 - x, l)) + "%"; }
  });
})();

// Période commune (SPEC § 57) : la période choisie sur une fiche machine,
// un tableau ou la page Logs est retenue par ce navigateur et reprise par
// les autres pages qui en ont une (seulement les périodes qu'elles ont
// toutes : 1h, 6h, 24h, 7d, 30d). Un lien qui en donne une (ou un zoom
// « de / a ») passe avant.
(function () {
  var cle = "mirador-periode", communes = ["1h", "6h", "24h", "7d", "30d"];
  var p = window.location.pathname;
  if (!/^\/(machines\/[^/]+|tableaux\/[^/]+|logs)$/.test(p)) { return; }
  var q = new URLSearchParams(window.location.search);
  var actuelle = q.get("periode");
  try {
    if (actuelle) {
      if (communes.indexOf(actuelle) >= 0) { window.localStorage.setItem(cle, actuelle); }
      return;
    }
    if (q.get("de") || q.get("avant") || q.get("motif")) { return; }
    var retenue = window.localStorage.getItem(cle);
    if (retenue && communes.indexOf(retenue) >= 0) {
      q.set("periode", retenue);
      window.location.replace(p + "?" + q.toString() + window.location.hash);
    }
  } catch (e) { /* stockage refusé : période par défaut */ }
})();
