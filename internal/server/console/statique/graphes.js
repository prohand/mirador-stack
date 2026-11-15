// SPDX-License-Identifier: GPL-3.0-or-later
// Courbes des tableaux : chaque <div data-courbe="URL"> reçoit ses données
// en JSON ({unite, x[], series[{nom, v[]}], seuil?}) et les dessine avec uPlot
// (MIT, embarqué : statique/uplot/). Rechargement toutes les 60 s, seulement
// si l'onglet est visible ; redimensionnement automatique (téléphone).
"use strict";

(function () {
  if (typeof uPlot === "undefined") { return; }
  var palette = ["#1f6feb", "#e16f24", "#1a7f37", "#cf222e", "#8250df", "#9a6700", "#0598bc", "#bf3989",
    "#57606a", "#2da44e", "#d4a72c", "#a475f9"];

  function echelle(v, base, suffixes, dec) {
    var i = 0, s = v < 0 ? -1 : 1;
    v = Math.abs(v);
    while (v >= base && i < suffixes.length - 1) { v /= base; i++; }
    return (s * v).toFixed(dec).replace(".", ",") + (suffixes[i] ? " " + suffixes[i] : "");
  }
  // Mêmes unités que le serveur (tableaux.go, formater).
  function formater(v, u) {
    if (v === null || v === undefined || isNaN(v)) { return "—"; }
    switch (u) {
      case "percent": return v.toFixed(1).replace(".", ",") + " %";
      case "percentunit": return (v * 100).toFixed(1).replace(".", ",") + " %";
      case "bytes": case "decbytes": return echelle(v, 1024, ["o", "Kio", "Mio", "Gio", "Tio"], 1);
      case "Bps": case "binBps": return echelle(v, 1024, ["o/s", "Kio/s", "Mio/s", "Gio/s"], 1);
      case "bps": return echelle(v, 1000, ["bit/s", "kbit/s", "Mbit/s", "Gbit/s"], 1);
      case "pps": return echelle(v, 1000, ["p/s", "kp/s", "Mp/s"], 1);
      case "s": case "dtdurations":
        if (Math.abs(v) < 120) { return v.toFixed(0) + " s"; }
        if (Math.abs(v) < 7200) { return (v / 60).toFixed(0) + " min"; }
        if (Math.abs(v) < 172800) { return (v / 3600).toFixed(1).replace(".", ",") + " h"; }
        return (v / 86400).toFixed(0) + " j";
      case "ms": return v.toFixed(0) + " ms";
      case "celsius": return v.toFixed(0) + " °C";
      case "lignes/min": return echelle(v, 1000, ["", "k", "M"], v < 10 ? 1 : 0) + " lignes/min";
      default: return echelle(v, 1000, ["", "k", "M", "G", "T"], Math.abs(v) < 10 ? 2 : 1);
    }
  }
  function couleurVar(nom) {
    return getComputedStyle(document.body).getPropertyValue(nom).trim() || "#888";
  }

  // Dates en français (légende au survol, axe des temps).
  var MOIS = ["janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.", "déc."];
  var JOURS = ["dim", "lun", "mar", "mer", "jeu", "ven", "sam"];
  function deux(n) { return (n < 10 ? "0" : "") + n; }
  function dateFr(t) {
    var d = new Date(t * 1000);
    return d.getDate() + " " + MOIS[d.getMonth()] + " " + deux(d.getHours()) + ":" + deux(d.getMinutes());
  }
  function axeTemps(u, vals) {
    var ecart = (u.scales.x.max || 0) - (u.scales.x.min || 0);
    return vals.map(function (v) {
      var d = new Date(v * 1000);
      if (ecart > 2 * 86400) { return JOURS[d.getDay()] + " " + d.getDate(); }
      return deux(d.getHours()) + ":" + deux(d.getMinutes());
    });
  }

  // Panneaux sans données regroupés en une ligne (page d'une machine :
  // conteneur data-regrouper-vides), SPEC § 29.
  function regrouperVides() {
    document.querySelectorAll("[data-regrouper-vides] .panneaux").forEach(function (pan) {
      var vides = Array.prototype.filter.call(pan.children, function (c) { return c.classList.contains("panneau-vide"); });
      var ligne = pan.querySelector(".sans-donnees");
      if (!vides.length) { if (ligne) { ligne.remove(); } return; }
      if (!ligne) { ligne = document.createElement("p"); ligne.className = "sans-donnees"; pan.appendChild(ligne); }
      ligne.textContent = "Pas de données sur la période : " + vides.map(function (c) {
        var h = c.querySelector("h2, h3");
        return h ? h.textContent.trim() : "";
      }).filter(Boolean).join(", ") + ".";
    });
  }
  function marquerVide(el, vide) {
    var carte = el.closest(".carte");
    if (carte && carte.closest("[data-regrouper-vides]")) { carte.classList.toggle("panneau-vide", vide); regrouperVides(); }
  }

  function dessiner(el) {
    // Zoom en cours (glisser sur la courbe) : pas de rechargement qui le
    // perdrait ; double-clic pour revenir.
    if (document.hidden || el.enCours || el.zoom) { return; }
    el.enCours = true;
    // data-courbe="apercu" : données déjà dans la page (aperçu de l'éditeur
    // de tableaux, panneau pas encore enregistré), dans data-json.
    var source = el.getAttribute("data-courbe");
    var donneesPretes = source === "apercu"
      ? Promise.resolve(JSON.parse(el.getAttribute("data-json") || "{}"))
      : fetch(source, { headers: { "X-Requested-With": "mirador" }, credentials: "same-origin" })
        .then(function (r) {
          if (r.status === 401) { window.location = "/connexion"; return null; }
          return r.json();
        });
    donneesPretes
      .then(function (d) {
        if (!d) { return; }
        if (!d.series || d.series.length === 0) {
          if (el.u) { el.u.destroy(); el.u = null; }
          el.textContent = d.erreur || "aucune donnée sur la période";
          el.classList.add("vide");
          marquerVide(el, true);
          return;
        }
        el.classList.remove("vide");
        marquerVide(el, false);
        var donnees = [d.x];
        var seule = d.series.length === 1;
        var series = [{ label: "Heure", value: function (u, v) { return v === null || v === undefined ? "—" : dateFr(v); } }];
        d.series.forEach(function (s, i) {
          var c = palette[i % palette.length];
          donnees.push(s.v);
          series.push({ label: s.nom, stroke: c, width: 1.75, spanGaps: false, fill: seule ? c + "22" : undefined,
            value: function (u, v) { return formater(v, d.unite); } });
        });
        var seuil = typeof d.seuil === "number" ? d.seuil : null;
        // Seuil loin au-dessus des valeurs (ex. 800 Mbit/s pour un trafic de
        // 2 Mbit/s) : pas tracé, il écraserait la courbe.
        if (seuil !== null && d.unite !== "percent" && d.unite !== "percentunit") {
          var maxi = 0;
          d.series.forEach(function (s) { s.v.forEach(function (v) { if (v !== null && Math.abs(v) > maxi) { maxi = Math.abs(v); } }); });
          if (seuil > 2 * maxi) { seuil = null; }
        }
        var texte = couleurVar("--doux"), grille = couleurVar("--bord"), rouge = couleurVar("--erreur");
        // Échelle : 0 à 100 % fixe pour les pourcentages ; le seuil reste visible.
        var plafond = d.unite === "percent" ? 100 : d.unite === "percentunit" ? 1 : null;
        var hauteur = el.closest("[data-regrouper-vides]") ? 200 : 220;
        var opts = {
          width: Math.max(el.clientWidth, 200), height: hauteur, series: series,
          legend: { show: d.series.length <= 12, live: true },
          cursor: { points: { show: false }, sync: { key: "mirador" }, drag: { x: true, y: false } },
          scales: { x: { time: true }, y: { range: function (u, mn, mx) {
            var haut = Math.max(mx, plafond === null ? mx : plafond, seuil === null ? mx : seuil * 1.1);
            var bas = Math.min(mn, 0); // depuis zéro : pas de petite variation grossie
            if (haut === bas) { haut = bas + 1; }
            return [bas, haut];
          } } },
          axes: [
            { stroke: texte, grid: { stroke: grille, width: 1 }, ticks: { stroke: grille }, values: axeTemps },
            { stroke: texte, grid: { stroke: grille, width: 1 }, ticks: { stroke: grille },
              // Largeur selon le plus long libellé (« 800,0 Mbit/s » entier).
              size: function (u, vals) {
                var n = 0;
                (vals || []).forEach(function (v) { n = Math.max(n, String(v).length); });
                return Math.max(48, n * 7 + 18);
              },
              values: function (u, vals) { return vals.map(function (v) { return formater(v, d.unite); }); } }
          ],
          hooks: {
            setSelect: [function (u) { if (u.select.width > 0) { el.zoom = true; } }],
            draw: [function (u) {
              if (seuil === null) { return; }
              var y = u.valToPos(seuil, "y", true), ctx = u.ctx;
              ctx.save();
              ctx.setLineDash([6, 4]);
              ctx.strokeStyle = rouge;
              ctx.lineWidth = 1.5 * (window.devicePixelRatio || 1);
              ctx.beginPath();
              ctx.moveTo(u.bbox.left, y);
              ctx.lineTo(u.bbox.left + u.bbox.width, y);
              ctx.stroke();
              ctx.restore();
            }]
          }
        };
        if (el.u) { el.u.destroy(); }
        el.textContent = "";
        el.u = new uPlot(opts, donnees, el);
        if (!el.ecouteZoom) {
          el.ecouteZoom = true;
          el.addEventListener("dblclick", function () { el.zoom = false; });
        }
        if (!el.indice && el.parentNode) {
          el.indice = document.createElement("p");
          el.indice.className = "indice-courbe";
          el.parentNode.insertBefore(el.indice, el.nextSibling);
        }
        if (el.indice) {
          el.indice.textContent = "Glisser pour zoomer · double-clic pour revenir" + (seuil !== null ? " · pointillé rouge : seuil du modèle" : "");
        }
        filtrer(el, d.series.length);
        if (d.erreur) {
          var p = document.createElement("small");
          p.textContent = d.erreur;
          el.appendChild(p);
        }
      })
      .catch(function () { el.textContent = "données indisponibles"; })
      .finally(function () { el.enCours = false; });
  }

  // Filtre des séries : à partir de 6 courbes (conteneurs, machines…), un
  // petit champ au-dessus du graphique ne garde que celles dont le nom
  // contient le texte tapé. Le choix est gardé d'un rechargement à l'autre.
  function appliquerFiltre(el) {
    if (!el.u) { return; }
    var mots = (el.filtre || "").toLowerCase().split(/\s+/).filter(Boolean);
    for (var i = 1; i < el.u.series.length; i++) {
      var nom = String(el.u.series[i].label || "").toLowerCase();
      var garde = mots.every(function (m) { return nom.indexOf(m) >= 0; });
      if (el.u.series[i].show !== garde) { el.u.setSeries(i, { show: garde }); }
    }
  }
  function filtrer(el, nb) {
    if (!el.champFiltre && nb >= 6 && el.parentNode) {
      var boite = document.createElement("div"), champ = document.createElement("input");
      boite.className = "filtre-series";
      champ.type = "search";
      champ.placeholder = "Filtrer les courbes…";
      champ.setAttribute("aria-label", "Filtrer les courbes de ce graphique");
      champ.maxLength = 63;
      champ.addEventListener("input", function () { el.filtre = champ.value; appliquerFiltre(el); });
      boite.appendChild(champ);
      el.parentNode.insertBefore(boite, el);
      el.champFiltre = champ;
    }
    if (el.filtre) { appliquerFiltre(el); }
  }

  function tout() { regrouperVides(); document.querySelectorAll("[data-courbe]").forEach(dessiner); }
  tout();
  setInterval(tout, 60000);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) { tout(); } });
  // Thème changé (sélecteur du menu) : courbes redessinées aux nouvelles couleurs.
  document.addEventListener("mirador:theme", tout);
  if (typeof ResizeObserver !== "undefined") {
    var ro = new ResizeObserver(function (entrees) {
      entrees.forEach(function (e) {
        var u = e.target.u;
        if (u && Math.abs(u.width - e.target.clientWidth) > 8) { u.setSize({ width: Math.max(e.target.clientWidth, 200), height: u.height }); }
      });
    });
    document.querySelectorAll("[data-courbe]").forEach(function (el) { ro.observe(el); });
  }
})();
