// SPDX-License-Identifier: GPL-3.0-or-later
// Courbes des tableaux : chaque <div data-courbe="URL"> reçoit ses données
// en JSON ({unite, x[], series[{nom, v[]}], seuil?}) et les dessine avec uPlot
// (MIT, embarqué : statique/uplot/). Rechargement toutes les 60 s, seulement
// si l'onglet est visible ; redimensionnement automatique (téléphone).
"use strict";

(function () {
  var cleComparer = "mirador-comparer";
  var comparerTout = false;
  try { comparerTout = window.localStorage.getItem(cleComparer) === "1"; } catch (e) { /* stockage refusé */ }
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

  // Âge des données (SPEC § 31) : pastille dans le titre de la carte,
  // verte « il y a 25 s », orange au-delà de 5 minutes.
  function texteAge(sec) {
    if (sec < 60) { return Math.max(0, Math.round(sec)) + " s"; }
    if (sec < 3600) { return Math.round(sec / 60) + " min"; }
    if (sec < 172800) { return Math.round(sec / 3600) + " h"; }
    return Math.round(sec / 86400) + " j";
  }
  function majAge(el) {
    var carte = el.closest(".carte"), titre = carte && carte.querySelector("h2, h3");
    if (!titre) { return; }
    var p = titre.querySelector(".age-donnees");
    if (!el.derniere) { if (p) { p.remove(); } return; }
    var age = Date.now() / 1000 - el.derniere, vieux = age > 300;
    // Sans heure de la machine (tableaux), seulement si les données manquent.
    if (!vieux && !el.ageSur) { if (p) { p.remove(); } return; }
    if (!p) { p = document.createElement("span"); titre.appendChild(p); }
    p.className = "age-donnees" + (vieux ? " age-vieux" : "");
    p.textContent = (vieux ? "dernière donnée il y a " : "il y a ") + texteAge(age);
    p.title = "Heure de la dernière donnée reçue : " + dateFr(el.derniere);
  }
  function derniereX(d) {
    var t = 0;
    d.series.forEach(function (s) {
      for (var i = s.v.length - 1; i >= 0; i--) { if (s.v[i] !== null) { t = Math.max(t, d.x[i]); break; } }
    });
    return t;
  }

  // Alertes (bandes) et événements (traits pointillés) d'une machine, SPEC § 31.
  function couleurGravite(g) {
    return couleurVar(g === "critical" || g === "critique" ? "--erreur" : g === "warning" || g === "attention" ? "--attention" : "--info");
  }
  function dessinerBandes(u, an) {
    if (!an || !an.alertes) { return; }
    var ctx = u.ctx, haut = u.bbox.top, h = u.bbox.height;
    ctx.save();
    an.alertes.forEach(function (a) {
      var x0 = Math.max(u.valToPos(a.debut, "x", true), u.bbox.left);
      var x1 = Math.min(u.valToPos(a.fin, "x", true), u.bbox.left + u.bbox.width);
      if (x1 <= x0) { return; }
      ctx.globalAlpha = 0.13;
      ctx.fillStyle = couleurGravite(a.gravite);
      ctx.fillRect(x0, haut, Math.max(x1 - x0, 2), h);
      ctx.globalAlpha = 0.9;
      ctx.fillRect(x0, haut, Math.max(x1 - x0, 2), 3 * (window.devicePixelRatio || 1));
    });
    ctx.restore();
  }
  function dessinerEvenements(u, an) {
    if (!an || !an.evenements) { return; }
    var ctx = u.ctx, r = window.devicePixelRatio || 1;
    ctx.save();
    ctx.setLineDash([3 * r, 3 * r]);
    ctx.strokeStyle = couleurVar("--pale");
    ctx.lineWidth = r;
    an.evenements.forEach(function (e) {
      var x = u.valToPos(e.t, "x", true);
      if (x < u.bbox.left || x > u.bbox.left + u.bbox.width) { return; }
      ctx.beginPath();
      ctx.moveTo(x, u.bbox.top);
      ctx.lineTo(x, u.bbox.top + u.bbox.height);
      ctx.stroke();
    });
    ctx.restore();
  }
  // Survol : nom de l'alerte ou de l'événement sous le curseur.
  function survolAnnotations(el, u, an) {
    if (!el.survol) { return; }
    var noms = [];
    if (an && u.cursor.left >= 0) {
      var t = u.posToVal(u.cursor.left, "x");
      (an.evenements || []).forEach(function (e) {
        if (Math.abs(u.valToPos(e.t, "x") - u.cursor.left) <= 6) { noms.push("┆ " + e.nom + " · " + dateFr(e.t)); }
      });
      (an.alertes || []).forEach(function (a) {
        if (t >= a.debut && t <= a.fin) { noms.push("▮ " + a.nom + " · " + dateFr(a.debut) + " → " + dateFr(a.fin)); }
      });
    }
    // Place gardée même vide (classe, pas « hidden ») : au survol, la page ne
    // bouge pas (retour du 20/11 : « la fenêtre remonte toute seule »).
    el.survol.textContent = noms.join("   ");
    el.survol.classList.toggle("vide", noms.length === 0);
  }

  // Zoom partagé par toutes les courbes de la page et gardé dans l'adresse
  // (« de » et « a », secondes Unix) : un lien copié montre la même vue.
  function zoomAdresse() {
    var q = new URLSearchParams(window.location.search), de = parseInt(q.get("de"), 10), a = parseInt(q.get("a"), 10);
    return de > 0 && a > de ? { de: de, a: a } : null;
  }
  function ecrireAdresse(de, a) {
    var q = new URLSearchParams(window.location.search);
    if (de) { q.set("de", String(de)); q.set("a", String(a)); } else { q.delete("de"); q.delete("a"); }
    var s = q.toString();
    window.history.replaceState(null, "", window.location.pathname + (s ? "?" + s : "") + window.location.hash);
  }
  function zoomerTout(source, de, a) {
    ecrireAdresse(de, a);
    document.querySelectorAll("[data-courbe]").forEach(function (el) {
      if (el !== source && el.u) { el.zoom = true; el.u.setScale("x", { min: de, max: a }); }
    });
  }
  function dezoomerTout() {
    ecrireAdresse(null);
    document.querySelectorAll("[data-courbe]").forEach(function (el) {
      if (el.u && el.zoom) { el.zoom = false; el.u.setScale("x", { min: el.u.data[0][0], max: el.u.data[0][el.u.data[0].length - 1] }); }
    });
  }

  // Barre d'outils de la carte (SPEC § 31) : semaine dernière, agrandir,
  // export PNG et CSV.
  function bouton(texte, titre, action) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "outil-courbe";
    b.textContent = texte;
    b.title = titre;
    b.setAttribute("aria-label", titre);
    b.addEventListener("click", action);
    return b;
  }
  function outils(el, d) {
    var carte = el.closest(".carte");
    if (!carte || el.getAttribute("data-courbe") === "apercu") { return; }
    if (!el.barreOutils) {
      var barre = document.createElement("div");
      barre.className = "outils-courbe";
      el.btnComparer = bouton("−7 j", "Comparer avec la même période il y a 7 jours", function () {
        el.comparer = !el.comparer;
        el.btnComparer.classList.toggle("actif", el.comparer);
        el.btnComparer.setAttribute("aria-pressed", el.comparer ? "true" : "false");
        var z = el.zoom; el.zoom = false; dessiner(el); el.zoom = z;
      });
      barre.appendChild(el.btnComparer);
      barre.appendChild(bouton("⤢", "Agrandir (Échap pour revenir)", function () { pleinEcran(el); }));
      barre.appendChild(bouton("PNG", "Enregistrer l'image de la courbe", function () { exporterPNG(el); }));
      barre.appendChild(bouton("CSV", "Enregistrer les valeurs (tableur)", function () { exporterCSV(el); }));
      carte.insertBefore(barre, carte.firstChild);
      el.barreOutils = barre;
    }
    // Comparaison : pour une seule courbe, pas pour les aires empilées.
    el.btnComparer.hidden = !(d.series.length === 1 && !d.empile);
  }
  function pleinEcran(el) {
    var carte = el.closest(".carte"), ouvert = !carte.classList.contains("plein-ecran");
    carte.classList.toggle("plein-ecran", ouvert);
    document.body.classList.toggle("avec-plein-ecran", ouvert);
    if (el.u) { el.u.setSize({ width: Math.max(el.clientWidth, 200), height: ouvert ? Math.max(window.innerHeight - 260, 260) : (el.closest("[data-regrouper-vides]") ? 200 : 220) }); }
  }
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") { return; }
    var c = document.querySelector(".carte.plein-ecran [data-courbe]");
    if (c) { pleinEcran(c); }
  });
  function nomFichier(el, ext) {
    var carte = el.closest(".carte"), h = carte && carte.querySelector("h2, h3");
    var titre = (h ? h.firstChild.textContent : "courbe").trim().toLowerCase()
      .normalize("NFD").replace(/[\u0300-\u036f]/g, "").replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
    var t = new Date();
    return (titre || "courbe") + "-" + t.getFullYear() + deux(t.getMonth() + 1) + deux(t.getDate()) + "-" + deux(t.getHours()) + deux(t.getMinutes()) + "." + ext;
  }
  function telecharger(blob, nom) {
    var a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = nom;
    document.body.appendChild(a);
    a.click();
    setTimeout(function () { URL.revokeObjectURL(a.href); a.remove(); }, 1000);
  }
  function exporterPNG(el) {
    if (!el.u) { return; }
    var src = el.u.ctx.canvas, c = document.createElement("canvas"), r = window.devicePixelRatio || 1;
    var carte = el.closest(".carte"), h = carte && carte.querySelector("h2, h3"), titre = h ? h.firstChild.textContent.trim() : "";
    var haut = Math.round(36 * r);
    c.width = src.width; c.height = src.height + haut;
    var ctx = c.getContext("2d");
    ctx.fillStyle = couleurVar("--carte"); ctx.fillRect(0, 0, c.width, c.height);
    ctx.fillStyle = couleurVar("--texte"); ctx.font = (15 * r) + "px sans-serif";
    ctx.fillText(titre + " · " + dateFr(el.u.scales.x.min) + " → " + dateFr(el.u.scales.x.max), 8 * r, 24 * r);
    ctx.drawImage(src, 0, haut);
    c.toBlob(function (b) { if (b) { telecharger(b, nomFichier(el, "png")); } }, "image/png");
  }
  // CSV pour un tableur français : « ; » entre colonnes, virgule décimale.
  function exporterCSV(el) {
    var d = el.donnees;
    if (!d) { return; }
    // Un nom de série vient des étiquettes envoyées par les machines : s'il
    // commence par = + - @ (ou tabulation, retour chariot), un tableur le
    // lirait comme une formule. Une apostrophe devant le garde en texte
    // (même règle que les exports du serveur).
    var cellule = function (t) {
      t = String(t);
      if (/^[=+\-@\t\r]/.test(t)) { t = "'" + t; }
      return /[";\n]/.test(t) ? "\"" + t.replace(/"/g, "\"\"") + "\"" : t;
    };
    var lignes = [["Heure"].concat(d.series.map(function (s) { return cellule(s.nom); })).concat(d.precedent ? ["Il y a 7 jours"] : []).join(";")];
    d.x.forEach(function (t, k) {
      var dt = new Date(t * 1000);
      var l = [dt.getFullYear() + "-" + deux(dt.getMonth() + 1) + "-" + deux(dt.getDate()) + " " + deux(dt.getHours()) + ":" + deux(dt.getMinutes()) + ":" + deux(dt.getSeconds())];
      d.series.forEach(function (s) { l.push(s.v[k] === null ? "" : String(s.v[k]).replace(".", ",")); });
      if (d.precedent) { l.push(d.precedent[k] === null ? "" : String(d.precedent[k]).replace(".", ",")); }
      lignes.push(l.join(";"));
    });
    telecharger(new Blob(["\ufeff" + lignes.join("\r\n") + "\r\n"], { type: "text/csv;charset=utf-8" }), nomFichier(el, "csv"));
  }

  // Aide des courbes : une seule fois par page, sous la première courbe
  // (avant : répétée sous chacune). Texte adapté au doigt : sur un écran
  // tactile on ne glisse pas pour zoomer (le geste fait défiler la page) et
  // on ne survole pas.
  var indicePage = null, indiceSeuil = false, indiceAn = false;
  var tactile = !!(window.matchMedia && window.matchMedia("(hover: none)").matches);
  function majIndice(el, seuil, avecAn) {
    if (!el.parentNode) { return; }
    indiceSeuil = indiceSeuil || seuil;
    indiceAn = indiceAn || avecAn;
    if (!indicePage || !document.contains(indicePage)) {
      indicePage = document.createElement("p");
      indicePage.className = "indice-courbe";
      // Sous la première courbe de la page (pas sous la première dessinée).
      var premiere = document.querySelector("[data-courbe]") || el;
      premiere.parentNode.insertBefore(indicePage, premiere.nextSibling);
    }
    indicePage.textContent = (tactile ? "Toucher une courbe : valeurs à cet instant" : "Glisser : zoom · double-clic : retour") +
      (indiceSeuil ? " · rouge : seuil du modèle" : "") +
      (indiceAn ? " · bandes : alertes · traits gris : événements" + (tactile ? "" : " (survoler)") : "");
  }

  // Courbes chargées à l'affichage (SPEC § 34.4) : une courbe n'est demandée
  // au serveur que lorsqu'elle approche de l'écran (400 px avant), puis
  // rafraîchie seulement tant qu'elle y reste. La page d'une machine a une
  // trentaine de courbes : sur téléphone on en voit une ou deux à la fois.
  // Une section repliée ne charge rien tant qu'elle n'est pas ouverte.
  var proches = typeof IntersectionObserver !== "undefined" ? new IntersectionObserver(function (entrees) {
    entrees.forEach(function (e) {
      e.target.proche = e.isIntersecting;
      if (e.isIntersecting && (!e.target.dessineLe || Date.now() - e.target.dessineLe > 60000)) { dessiner(e.target); }
    });
  }, { rootMargin: "400px 0px" }) : null;

  function dessiner(el) {
    // Zoom en cours (glisser sur la courbe) : pas de rechargement qui le
    // perdrait ; double-clic pour revenir.
    if (document.hidden || el.enCours || el.zoom) { return; }
    // Loin de l'écran : rien à demander (voir « proches » ci-dessus).
    if (proches && !el.proche) { return; }
    el.enCours = true;
    // data-courbe="apercu" : données déjà dans la page (aperçu de l'éditeur
    // de tableaux, panneau pas encore enregistré), dans data-json.
    var source = el.getAttribute("data-courbe");
    // Comparaison réglée pour toute la page (SPEC § 57) : reprise par chaque
    // courbe au premier dessin ; le bouton « −7 j » la change pour une seule.
    if (el.comparer === undefined) { el.comparer = comparerTout; }
    if (el.btnComparer) { el.btnComparer.classList.toggle("actif", !!el.comparer); }
    if (el.comparer && source !== "apercu") { source += "&comparer=1"; }
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
          el.textContent = d.erreur || "Aucune donnée sur la période";
          el.classList.add("vide");
          marquerVide(el, true);
          return;
        }
        el.classList.remove("vide");
        marquerVide(el, false);
        var donnees = [d.x];
        var seule = d.series.length === 1;
        var series = [{ label: "Heure", value: function (u, v) { return v === null || v === undefined ? "—" : dateFr(v); } }];
        // Aires empilées (SPEC § 31) : chaque courbe posée sur la précédente ;
        // la légende montre la valeur propre de chaque courbe.
        var empile = !!d.empile && d.series.length > 1, cumul = null, bandes = [];
        d.series.forEach(function (s, i) {
          var c = palette[i % palette.length], v = s.v;
          if (empile) {
            cumul = v.map(function (x, k) {
              var bas = cumul ? cumul[k] : 0;
              return x === null ? bas : (bas === null ? 0 : bas) + x;
            });
            v = cumul;
            if (i > 0) { bandes.push({ series: [i + 1, i], fill: c + "55" }); }
          }
          donnees.push(v);
          series.push({ label: s.nom, stroke: c, width: empile ? 1 : 1.75, spanGaps: false,
            fill: empile ? (i === 0 ? c + "55" : undefined) : (seule ? c + "22" : undefined),
            value: function (u, x, si, k) { return formater(k === null || k === undefined ? x : s.v[k], d.unite); } });
        });
        // Semaine dernière : même période 7 jours plus tôt, en pointillé gris.
        if (d.precedent && seule) {
          donnees.push(d.precedent);
          series.push({ label: "Il y a 7 jours", stroke: couleurVar("--pale"), width: 1.25, dash: [5, 4], spanGaps: false,
            value: function (u, v) { return formater(v, d.unite); } });
        }
        el.donnees = d;
        el.dessineLe = Date.now();
        // Dernier instant qui a une valeur : la légende le montre tant que
        // le curseur n'est pas sur la courbe (avant : « — » partout).
        var dernier = -1;
        for (var k = d.x.length - 1; k >= 0 && dernier < 0; k--) {
          for (var j = 0; j < d.series.length; j++) { if (d.series[j].v[k] !== null) { dernier = k; break; } }
        }
        var an = d.annotations || null;
        el.ageSur = !!(an && an.derniere);
        el.derniere = el.ageSur ? an.derniere : derniereX(d);
        majAge(el);
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
        var hauteur = el.closest(".plein-ecran") ? Math.max(window.innerHeight - 260, 260) : el.closest("[data-regrouper-vides]") ? 200 : 220;
        var opts = {
          width: Math.max(el.clientWidth, 200), height: hauteur, series: series,
          legend: { show: d.series.length <= 12, live: true },
          bands: bandes,
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
            setSelect: [function (u) {
              if (u.select.width > 0) {
                el.zoom = true;
                var de = Math.round(u.posToVal(u.select.left, "x")), a = Math.round(u.posToVal(u.select.left + u.select.width, "x"));
                zoomerTout(el, de, a);
              }
            }],
            setCursor: [function (u) {
              survolAnnotations(el, u, an);
              if ((u.cursor.idx === null || u.cursor.idx === undefined) && dernier >= 0) { u.setLegend({ idx: dernier }, false); }
            }],
            drawClear: [function (u) { dessinerBandes(u, an); }],
            draw: [function (u) {
              dessinerEvenements(u, an);
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
        if (dernier >= 0) { el.u.setLegend({ idx: dernier }, false); }
        if (!el.ecouteZoom) {
          el.ecouteZoom = true;
          el.addEventListener("dblclick", function () { dezoomerTout(); });
        }
        var avecAn = an && ((an.alertes && an.alertes.length) || (an.evenements && an.evenements.length));
        if (avecAn && !el.survol && el.parentNode) {
          el.survol = document.createElement("p");
          el.survol.className = "survol-annotation vide";
          el.parentNode.insertBefore(el.survol, el.nextSibling);
        }
        majIndice(el, seuil !== null, !!avecAn);
        filtrer(el, d.series.length);
        outils(el, d);
        // Zoom donné par l'adresse (lien partagé) : appliqué au premier dessin.
        var z = zoomAdresse();
        if (z && z.de >= d.x[0] && z.a <= d.x[d.x.length - 1] + 3600) {
          el.zoom = true;
          el.u.setScale("x", { min: z.de, max: z.a });
        }
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

  // « Comparer avec la semaine dernière » pour toutes les courbes de la
  // page, retenu par ce navigateur (SPEC § 57). Case posée à côté du choix
  // de la période (fiche machine, tableau).
  function poserComparerTout() {
    var courbes = document.querySelectorAll("[data-courbe]:not([data-courbe=apercu])");
    var ancre = document.querySelector("nav .segment.periode, form select[name=periode]");
    if (!courbes.length || !ancre) { return; }
    var lab = document.createElement("label");
    lab.className = "case comparer-tout";
    var cb = document.createElement("input");
    cb.type = "checkbox";
    cb.checked = comparerTout;
    lab.appendChild(cb);
    lab.appendChild(document.createTextNode(" Comparer avec la semaine dernière"));
    lab.title = "Toutes les courbes : la même période il y a 7 jours, en pointillé gris (une seule courbe à la fois)";
    cb.addEventListener("change", function () {
      comparerTout = cb.checked;
      try { window.localStorage.setItem(cleComparer, comparerTout ? "1" : "0"); } catch (e) { /* stockage refusé */ }
      courbes.forEach(function (el) { el.comparer = comparerTout; var z = el.zoom; el.zoom = false; dessiner(el); el.zoom = z; });
    });
    var cible = ancre.closest("label") || ancre;
    cible.insertAdjacentElement("afterend", lab);
  }
  poserComparerTout();
  tout();
  setInterval(tout, 60000);
  // Pastilles d'âge tenues à jour entre deux rechargements.
  setInterval(function () { document.querySelectorAll("[data-courbe]").forEach(majAge); }, 15000);
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
  if (proches) { document.querySelectorAll("[data-courbe]").forEach(function (el) { proches.observe(el); }); }
})();
