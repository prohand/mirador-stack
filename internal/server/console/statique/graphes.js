// SPDX-License-Identifier: GPL-3.0-or-later
// Courbes des tableaux : chaque <div data-courbe="URL"> reçoit ses données
// en JSON ({unite, x[], series[{nom, v[]}]}) et les dessine avec uPlot
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

  function dessiner(el) {
    if (document.hidden || el.enCours) { return; }
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
          return;
        }
        el.classList.remove("vide");
        var donnees = [d.x];
        var series = [{}];
        d.series.forEach(function (s, i) {
          donnees.push(s.v);
          series.push({ label: s.nom, stroke: palette[i % palette.length], width: 1.25, spanGaps: false,
            value: function (u, v) { return formater(v, d.unite); } });
        });
        var texte = couleurVar("--doux"), grille = couleurVar("--bord");
        var opts = {
          width: Math.max(el.clientWidth, 200), height: 220, series: series,
          legend: { show: d.series.length <= 12, live: true },
          cursor: { points: { show: false } },
          scales: { x: { time: true } },
          axes: [
            { stroke: texte, grid: { stroke: grille, width: 1 }, ticks: { stroke: grille } },
            { stroke: texte, grid: { stroke: grille, width: 1 }, ticks: { stroke: grille }, size: 72,
              values: function (u, vals) { return vals.map(function (v) { return formater(v, d.unite); }); } }
          ]
        };
        if (el.u) { el.u.destroy(); }
        el.textContent = "";
        el.u = new uPlot(opts, donnees, el);
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

  function tout() { document.querySelectorAll("[data-courbe]").forEach(dessiner); }
  tout();
  setInterval(tout, 60000);
  document.addEventListener("visibilitychange", function () { if (!document.hidden) { tout(); } });
  if (typeof ResizeObserver !== "undefined") {
    var ro = new ResizeObserver(function (entrees) {
      entrees.forEach(function (e) {
        var u = e.target.u;
        if (u && Math.abs(u.width - e.target.clientWidth) > 8) { u.setSize({ width: Math.max(e.target.clientWidth, 200), height: 220 }); }
      });
    });
    document.querySelectorAll("[data-courbe]").forEach(function (el) { ro.observe(el); });
  }
})();
