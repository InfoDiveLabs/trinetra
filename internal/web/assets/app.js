/* trinetra web UI -- shared client behavior.
 *
 * Ported from ui-mockup/assets/app.js. The mockup injected the sidebar/topbar
 * shell client-side (from a NAV array + data-shell attributes) and demoed
 * role-switching via a `.mockbar` + localStorage; the shell is now rendered
 * server-side (see internal/web/templates/base.html, the "nav" block) with
 * the real role coming from the session (stubbed to "admin" until Task
 * 5/6's auth lands), so all of that is stripped here. Kept: theme toggle,
 * the SVG gradient defs, the heartbeat/sparkline helpers, and the drawer/
 * modal/tabs/filter/switch interaction handlers -- later tasks (dashboard/
 * monitoring/history/channels pages) reuse these once they render real
 * markup into the same hooks. The mockup's #hbstrip (availability strip)
 * demo builder was removed once the real 24h up/down data was computed
 * server-side (internal/web/availability.go's ComputeAvailability,
 * dashboard.html's #hbstrip markup) instead of faked here.
 */
(function(){
  // ---- chart colors ----
  // uPlot series draw to a <canvas>, whose 2D context can't resolve CSS
  // custom properties: assigning strokeStyle='var(--info)' silently no-ops
  // (the context keeps its previous/default color) instead of resolving the
  // token, which is why the history/dashboard line charts rendered with
  // effectively invisible strokes in both themes. Chart lines therefore use
  // this fixed, theme-independent bright palette rather than var(--…)
  // tokens; keys mirror the style.css custom properties they stand in for
  // (--pink/--info/--cyan/--violet) so the mapping stays recognizable next to
  // the legend swatches (those ARE plain DOM elements and can use var(--…)
  // directly, since normal CSS -- not a canvas context -- resolves them).
  // Data series never use ember (--signal/--crit) or amber (--warn): in the
  // Trinetra palette those mean "look here" / "lagging", not "a metric".
  var CHART_COLORS={pink:'#e87ba6',info:'#4aa3ff',cyan:'#34d399',violet:'#a78bfa'};

  // ---- axis colors (theme-aware) ----
  // Unlike a series' own stroke/fill (CHART_COLORS above), uPlot's axis
  // label/tick/grid options are plain style reads, not canvas fills, so
  // var(--…) custom properties DO resolve here via getComputedStyle --
  // uPlot's own default (an empty axes:[{},{}]) is a fixed dark-mode color
  // that's near-invisible against this app's dark background, so resolve
  // the muted-text/hairline-border tokens style.css actually defines
  // (--muted, --border) instead of leaving it at that default.
  function swAxisColors(){
    var cs=getComputedStyle(document.documentElement);
    var muted=(cs.getPropertyValue('--muted')||'#8A949E').trim();
    var border=(cs.getPropertyValue('--border')||'rgba(255,255,255,.08)').trim();
    return {stroke:muted,grid:border};
  }
  // swAxesOpt returns a fresh uPlot `axes` array (one entry per x/y axis,
  // both charts here only ever have the two) colored from the CURRENTLY
  // active theme -- call it at chart-build time, not once at load, so a
  // rebuild after a theme toggle (see the 'sw-theme' listeners below) picks
  // up the new theme's colors.
  function swAxesOpt(){
    var c=swAxisColors();
    var ax={stroke:c.stroke,ticks:{stroke:c.grid},grid:{stroke:c.grid}};
    return [ax,ax];
  }

  // ---- responsive charts ----
  // uPlot draws to a fixed-pixel <canvas>, so a chart built at one container
  // width overflows (or under-fills) after a viewport resize or a phone
  // rotation -- most visible on mobile, where the single-column layout width
  // changes a lot. Both chart boots (swBootSSE, swBootHistoryCharts) register
  // their instances here keyed by mount-element id; a debounced global handler
  // calls uPlot's setSize() to refit each to its container. Keying by id means
  // a rebuild (e.g. the theme-toggle destroy+recreate) overwrites the stale
  // entry rather than leaking it. Guarded by isConnected + try/catch so a
  // just-destroyed instance can't throw.
  var swChartReg={};
  function swRegisterChart(u,el){ if(u&&el&&el.id) swChartReg[el.id]={u:u,el:el}; }
  function swResizeCharts(){
    Object.keys(swChartReg).forEach(function(id){
      var c=swChartReg[id];
      if(!c||!c.el||!c.el.isConnected){ delete swChartReg[id]; return; }
      var w=c.el.clientWidth;
      if(w>0){ try{ c.u.setSize({width:w,height:c.el.clientHeight||c.u.height||160}); }catch(e){ delete swChartReg[id]; } }
    });
  }
  var swResizeTO;
  function swScheduleResize(){ clearTimeout(swResizeTO); swResizeTO=setTimeout(swResizeCharts,120); }
  window.addEventListener('resize',swScheduleResize);
  window.addEventListener('orientationchange',swScheduleResize);
  window.addEventListener('load',swScheduleResize);

  // ---- gradient defs ----
  document.body.insertAdjacentHTML('afterbegin','<svg width="0" height="0" style="position:absolute" aria-hidden="true"><defs>'+
    ['gInfo','gViolet','gOk','gCrit','gCyan'].map(function(id){return '<linearGradient id="'+id+'" x1="0" y1="0" x2="0" y2="1"><stop class="a" offset="0"/><stop class="b" offset="1"/></linearGradient>';}).join('')+'</defs></svg>');

  // ---- theme ----
  // setTheme dispatches a 'sw-theme' DOM event after applying the new
  // data-theme so any already-built uPlot charts can re-init with the new
  // theme's axis colors (swAxesOpt) -- see swBootSSE/swBootHistoryCharts'
  // 'sw-theme' listeners below, which destroy+rebuild their charts on it.
  function setTheme(t){document.documentElement.dataset.theme=t; try{localStorage.sw_theme=t}catch(e){} document.dispatchEvent(new Event('sw-theme'));}
  try{ if(localStorage.sw_theme) setTheme(localStorage.sw_theme); else if(matchMedia('(prefers-color-scheme:light)').matches) setTheme('light'); else setTheme('dark'); }catch(e){setTheme('dark');}
  window.swToggleTheme=function(){setTheme(document.documentElement.dataset.theme==='dark'?'light':'dark');};
  document.addEventListener('keydown',function(e){ if((e.key==='t'||e.key==='T') && !/input|textarea|select/i.test(document.activeElement.tagName)) window.swToggleTheme(); });
  // Wire the topbar theme button here rather than via an inline onclick=:
  // the strict Content-Security-Policy (internal/web/security.go,
  // script-src 'self' 'nonce-…' with no unsafe-inline) blocks inline event
  // handlers, so base.html carries id="themeBtn" and we bind it in JS.
  var themeBtn=document.getElementById('themeBtn');
  if(themeBtn) themeBtn.addEventListener('click',window.swToggleTheme);

  // ---- heartbeat mini ----
  function ekg(mid,sp,count,W,H,gF,gT){var d='M0,'+mid,i,x;for(i=0;i<count;i++){x=10+i*sp;if(x>=gF&&x<=gT){d+=' L'+x+','+mid;continue;}d+=' L'+(x-6)+','+mid+' L'+(x-3)+','+(mid-4)+' L'+x+','+(mid-H*0.55)+' L'+(x+3)+','+(mid+6)+' L'+(x+6)+','+mid;}d+=' L'+W+','+mid;return d;}
  document.querySelectorAll('.hb-gen').forEach(function(el){el.innerHTML='<svg width="120" height="24" viewBox="0 0 120 24" aria-hidden="true"><path class="lead" stroke-width="1.6" d="'+ekg(13,26,4,120,24,999,999)+'"/><circle class="dot live" cx="115" cy="13" r="2.6"/></svg>';});

  // ---- uptime bars (public) ----
  var upb=document.getElementById('upbars');
  if(upb){var h='',k;for(k=0;k<90;k++){var c='var(--ok)',ht=38;if(k===69){c='var(--crit)'}else if(k===72){c='var(--warn)';ht=30}h+='<div style="flex:1;height:'+ht+'px;background:'+c+';opacity:.85;border-radius:2px"></div>';}upb.innerHTML=h;}

  // ---- node-scoped data URLs (Task 4: fleet dashboard, remote nodes) ----
  // A master's page can be its OWN view ("self", <body data-node-prefix="">)
  // or its view of a REMOTE fleet node (<body data-node-prefix="/n/{id}">,
  // set by templates/base.html from PageData.Node.Prefix). Every same-origin
  // fetch()/EventSource() this file makes against THIS daemon's own data API
  // (/api/series, /api/container/logs, /api/downtime, /events) must go
  // through nodeURL so a remote node's page reads THAT node's data instead
  // of silently hitting the literal absolute path, which always means "this
  // server" -- i.e. the master. /public/events, /enroll/*, /login/*,
  // /logout are deliberately never passed through nodeURL: those are
  // master-local endpoints (node_scope.go's masterLocalPrefixes) that have
  // no per-node counterpart to route to in the first place.
  function nodeURL(path){
    var prefix=(document.body&&document.body.dataset.nodePrefix)||'';
    return prefix+path;
  }

  // ---- detail drawer ----
  document.body.insertAdjacentHTML('beforeend','<div class="drawer-scrim" id="swScrim"></div><aside class="drawer" id="swDrawer"></aside>');
  var drawer=document.getElementById('swDrawer'), dScrim=document.getElementById('swScrim');
  function closeDrawer(){drawer.classList.remove('on');dScrim.classList.remove('on');}
  dScrim.addEventListener('click',closeDrawer);
  document.addEventListener('keydown',function(e){if(e.key==='Escape')closeDrawer();});
  // el builds a node with a class and, when txt is given, sets it via
  // textContent so untrusted data-* values are never parsed as HTML.
  function mkEl(tag,cls,txt){var n=document.createElement(tag); if(cls)n.className=cls; if(txt!=null)n.textContent=txt; return n;}
  // drawSpark renders an /api/series response ([ts[],avg[],min[],max[]]) into a
  // canvas: a faint min/max band with the avg line over it. Pure Canvas 2D (no
  // uPlot) so it works in the drawer without a layout pass. Reads the accent and
  // border colors from CSS custom properties so it tracks the active theme.
  function drawSpark(cv,s){
    var ctx=cv.getContext('2d'); if(!ctx) return;
    var W=cv.width, H=cv.height, pad=6;
    ctx.clearRect(0,0,W,H);
    var avg=s[1]||[], mn=s[2]||avg, mx=s[3]||avg, n=avg.length; if(!n) return;
    var lo=Infinity, hi=-Infinity, i;
    for(i=0;i<n;i++){ if(mn[i]<lo)lo=mn[i]; if(mx[i]>hi)hi=mx[i]; }
    if(!isFinite(lo)||!isFinite(hi)){ return; }
    if(hi-lo<1e-9){ hi=lo+1; } // flat series: give the band a little height
    var cs=getComputedStyle(document.body);
    var accent=(cs.getPropertyValue('--info')||'#5AA9E6').trim();
    var border=(cs.getPropertyValue('--border')||'rgba(128,128,128,.3)').trim();
    function X(i){ return pad+(W-2*pad)*(n===1?0.5:i/(n-1)); }
    function Y(v){ return H-pad-(H-2*pad)*((v-lo)/(hi-lo)); }
    // min/max band
    ctx.beginPath(); ctx.moveTo(X(0),Y(mx[0]));
    for(i=1;i<n;i++)ctx.lineTo(X(i),Y(mx[i]));
    for(i=n-1;i>=0;i--)ctx.lineTo(X(i),Y(mn[i]));
    ctx.closePath(); ctx.fillStyle=accent+'22'; ctx.fill();
    // avg line
    ctx.beginPath(); ctx.moveTo(X(0),Y(avg[0]));
    for(i=1;i<n;i++)ctx.lineTo(X(i),Y(avg[i]));
    ctx.strokeStyle=accent; ctx.lineWidth=1.5; ctx.stroke();
    // baseline
    ctx.strokeStyle=border; ctx.lineWidth=1; ctx.beginPath();
    ctx.moveTo(pad,H-pad); ctx.lineTo(W-pad,H-pad); ctx.stroke();
  }
  // staticInto appends parsed STATIC markup (no user data) into parent. Used
  // only for the fixed chart/action templates below, never for data-* values.
  function staticInto(parent,html){var t=document.createElement('div'); t.innerHTML=html; while(t.firstChild)parent.appendChild(t.firstChild);}
  document.addEventListener('click',function(e){
    var row=e.target.closest('[data-detail]'); if(!row) return;
    var d=row.dataset, kind=d.kind||'item';
    var role=document.body.dataset.role||'admin';

    // Build the drawer with createElement/textContent so untrusted data-*
    // values (name, state, and the k/v detail pairs) are never assigned to
    // innerHTML (#94). Only the fixed chart/action templates, which carry no
    // user data, are still parsed as HTML (via staticInto).
    var head=mkEl('div','dh');
    var title=mkEl('div'); title.style.flex='1';
    title.appendChild(mkEl('div','eyebrow',kind));
    var h3=mkEl('h3',null,d.name||''); h3.style.margin='2px 0 0'; h3.style.fontSize='16px';
    title.appendChild(h3);
    head.appendChild(title);
    if(d.state){
      var sc=(d.state==='running'||d.state==='active'||d.state==='PASS')?'ok':(d.state==='restarting'?'warn':'crit');
      head.appendChild(mkEl('span','badge '+sc,d.state));
    }
    var closeBtn=mkEl('button','icon-btn','✕'); closeBtn.id='swClose';
    head.appendChild(closeBtn);

    var body=mkEl('div','db');
    var dl=mkEl('dl','kv');
    Object.keys(d).forEach(function(k){ if(['detail','name','kind','state','metric'].indexOf(k)>-1)return; dl.appendChild(mkEl('dt',null,k)); dl.appendChild(mkEl('dd',null,d[k])); });
    body.appendChild(dl);

    // Per-metric history (#115): when the row names a stored series (data-metric,
    // e.g. "cpu", "mem", "disk:/data"), draw a real sparkline from /api/series
    // over the last 6h. Rows without a stored series (containers, services,
    // processes) show no chart section -- their live values are in the kv list
    // above -- rather than a decorative chart with no data (#114).
    if(d.metric){
      staticInto(body,'<div class="section-label"><span class="eyebrow">History · 6h</span></div>');
      var cv=mkEl('canvas','spark'); cv.width=520; cv.height=90; cv.style.width='100%'; cv.style.height='90px';
      body.appendChild(cv);
      var note=mkEl('div','note'); note.style.padding='4px 0'; note.textContent='Loading history...'; body.appendChild(note);
      var to=Math.floor(Date.now()/1000), from=to-6*3600;
      fetch(nodeURL('/api/series?metric='+encodeURIComponent(d.metric)+'&from='+from+'&to='+to),{credentials:'same-origin'})
        .then(function(r){ if(!r.ok) throw new Error('HTTP '+r.status); return r.json(); })
        .then(function(j){
          var s=j&&j.series; if(!s||!s.length||!s[0]||!s[0].length){ note.textContent='No history recorded yet.'; cv.style.display='none'; return; }
          drawSpark(cv,s); note.style.display='none';
        })
        .catch(function(err){ note.textContent='Could not load history: '+err.message; cv.style.display='none'; });
    }

    // Actions: the Restart button is gone (a monitor is not a container control
    // plane, and for Swarm services Swarm owns task lifecycle, #114). "View
    // logs" is now wired to /api/container/logs (#115); "Pause monitoring" is
    // still parked and renders disabled rather than as a dead button.
    staticInto(body,'<div class="section-label"><span class="eyebrow">Actions</span></div>');
    var actionRow=mkEl('div'); actionRow.style.display='flex'; actionRow.style.gap='8px'; actionRow.style.flexWrap='wrap';
    var logBtn=null;
    if(role==='admin'){
      if(kind==='container'){
        logBtn=mkEl('button','btn ghost','View logs'); actionRow.appendChild(logBtn);
      }
      staticInto(actionRow,'<button class="btn ghost" disabled title="Not available yet">Pause monitoring</button>');
    } else {
      staticInto(actionRow,'<span class="note">viewer -- read-only</span>');
    }
    body.appendChild(actionRow);
    // Log output area, filled on demand when "View logs" is clicked. Built with
    // textContent so container output (untrusted) is never parsed as HTML.
    var logBox=null;
    if(logBtn){
      logBox=mkEl('pre','logbox'); logBox.style.display='none';
      body.appendChild(logBox);
      logBtn.addEventListener('click',function(){
        logBtn.disabled=true; var orig=logBtn.textContent; logBtn.textContent='Loading...';
        // Spinner (task C6): a remote node's "View logs" goes over an RPC
        // round trip that can take up to 10s (fleet_rpc.go's fixed
        // timeout) before resolving either way, so the drawer shows a
        // spinner + "Loading logs…" the whole time rather than a blank box
        // -- built with createElement/textContent, no innerHTML, matching
        // this handler's existing untrusted-data discipline.
        logBox.style.display='block'; logBox.innerHTML='';
        var spin=mkEl('span','logbox-spinner');
        spin.appendChild(mkEl('span','spinner'));
        spin.appendChild(document.createTextNode('Loading logs…'));
        logBox.appendChild(spin);
        fetch(nodeURL('/api/container/logs?tail=200&name='+encodeURIComponent(d.name||'')),{credentials:'same-origin'})
          .then(function(r){
            // A remote node's stream/RPC-level failure (containerLogsHandler,
            // task C6: 503 node-not-connected, 504 RPC-timed-out-at-10s, 429
            // node-busy) carries the exact reason as JSON {"error": "..."} --
            // show that reason text directly rather than a generic "HTTP
            // 503"/"HTTP 504"/"HTTP 429".
            if(r.status===503||r.status===504||r.status===429){
              return r.json().catch(function(){ return {}; }).then(function(j){
                var e=new Error((j&&j.error)||('HTTP '+r.status));
                e.isReason=true; throw e;
              });
            }
            if(!r.ok) throw new Error('HTTP '+r.status);
            return r.text();
          })
          .then(function(t){ logBox.textContent=t||'(no output)'; logBox.scrollTop=logBox.scrollHeight; })
          .catch(function(err){ logBox.textContent=err.isReason?err.message:('Could not load logs: '+err.message); })
          .finally(function(){ logBtn.disabled=false; logBtn.textContent=orig; });
      });
    }

    drawer.innerHTML='';
    drawer.appendChild(head); drawer.appendChild(body);
    document.getElementById('swClose').addEventListener('click',closeDrawer);
    drawer.classList.add('on'); dScrim.classList.add('on');
  });

  // ---- modals ----
  document.addEventListener('click',function(e){
    var open=e.target.closest('[data-modal]');
    if(open){var m=document.getElementById(open.dataset.modal); if(m){ if(open.dataset.chanName) prefillChannel(m,open.dataset); else resetChannel(m); m.classList.add('on'); }}
    if(e.target.closest('[data-close]')||e.target.classList.contains('modal')){document.querySelectorAll('.modal.on').forEach(function(m){m.classList.remove('on')});}
  });
  function prefillChannel(m,d){var h=m.querySelector('.mh h3'); if(h)h.textContent='Edit channel'; var name=m.querySelector('input'); if(name)name.value=d.chanName||''; var sel=m.querySelector('[data-cond-src]'); if(sel&&d.chanType){sel.value=d.chanType; sel.dispatchEvent(new Event('change'));}}
  function resetChannel(m){var h=m.querySelector('.mh h3'); if(h&&h.textContent==='Edit channel')h.textContent='Add channel';}
  document.addEventListener('change',function(e){ if(e.target.matches('[data-cond-src]')){var v=e.target.value; document.querySelectorAll('[data-cond]').forEach(function(el){el.classList.toggle('on',el.dataset.cond===v);});} });
  // [data-autosubmit] (fleet.html's state filter <select>): submits its
  // form on change, a GET-only plain-navigation equivalent of "auto-apply
  // this filter" -- delegated through document, the same convention every
  // other generic behavior here uses, rather than an inline onchange=
  // attribute (the CSP's script-src has no 'unsafe-inline' -- security.go
  // -- so an inline event-handler attribute would simply never run).
  document.addEventListener('change',function(e){ if(e.target.matches('[data-autosubmit]')&&e.target.form){ e.target.form.submit(); } });

  // ---- tabs / filter / chips / switches ----
  document.addEventListener('click',function(e){var b=e.target.closest('.tabs button');if(!b)return;var w=b.closest('[data-tabs]');w.querySelectorAll('.tabs button').forEach(function(x){x.classList.toggle('on',x===b)});w.querySelectorAll('.tabpane').forEach(function(p){p.classList.toggle('on',p.dataset.pane===b.dataset.tab)});});
  document.querySelectorAll('[data-filter]').forEach(function(inp){inp.addEventListener('input',function(){var q=inp.value.toLowerCase();document.querySelectorAll(inp.dataset.filter).forEach(function(tbl){tbl.querySelectorAll('tbody tr').forEach(function(tr){tr.style.display=tr.textContent.toLowerCase().indexOf(q)>-1?'':'none';});});});});
  document.addEventListener('click',function(e){var c=e.target.closest('.chip');if(c&&c.parentElement&&c.parentElement.classList.contains('chips')){c.parentElement.querySelectorAll('.chip').forEach(function(x){x.classList.remove('on')});c.classList.add('on');}});
  // fleet admin (task 7): a generic "copy this element's text" button --
  // data-copy names the id of the element to copy (fleet_admin.html's
  // join-code <code> box). Delegated + always registered (like the chip/
  // tabs handlers above) rather than gated on a page-specific root element,
  // since any future page can reuse the same [data-copy] hook. Falls back
  // to a hidden-textarea + execCommand('copy') when the async Clipboard API
  // isn't available (non-secure context, or an older browser).
  document.addEventListener('click',function(e){
    var b=e.target.closest('[data-copy]'); if(!b) return;
    var target=document.getElementById(b.getAttribute('data-copy')); if(!target) return;
    var text=target.textContent||'';
    var flash=function(){ var prev=b.textContent; b.textContent='Copied'; setTimeout(function(){ b.textContent=prev; },1500); };
    if(navigator.clipboard && navigator.clipboard.writeText){
      navigator.clipboard.writeText(text).then(flash,function(){});
      return;
    }
    var ta=document.createElement('textarea');
    ta.value=text; ta.style.position='fixed'; ta.style.opacity='0';
    document.body.appendChild(ta); ta.select();
    try{ document.execCommand('copy'); flash(); }catch(err){}
    document.body.removeChild(ta);
  });
  // A real `<input type="checkbox" class="switch">` (every server-rendered
  // switch in config.html/public_settings.html/channels.html's modal forms,
  // plus /settings/public's "enabled" + "panel" checkboxes) is driven purely
  // by CSS `:checked` (style.css's input.switch:checked rule) -- the browser
  // already toggles its checked state, and the visual, on click, with no JS
  // needed at all. Toggling a `.on` class on it too (the mockup's original
  // behavior, ported unmodified) went out of sync with that: an input that
  // starts checked (class list ["switch"], no "on") whose FIRST click
  // unchecks it natively (checked -> false) would ALSO gain the "on" class
  // (absent -> present), and .switch.on's background rule has no ":checked"
  // guard, so it kept painting the switch as on despite the real control now
  // being unchecked -- exactly the reported "toggle doesn't visually update
  // until Save" bug (a full-page reload re-renders from the server's actual
  // value and "fixes" it, which is what made it look like only Save could
  // ever update it). Scoping this handler off real inputs entirely fixes
  // that: `:checked` alone drives every real switch's look, correctly and
  // immediately, on every click. What's left for this handler is the one
  // genuinely decorative `.switch` this app still has -- channels.html's
  // per-row enable toggle, a `<button type="submit">` (not an input) whose
  // class carries its OWN state via a server-rendered `{{if $c.Enabled}}on{{
  // end}}`; giving it the same instant-feedback toggle before its form's
  // full-page submit completes is harmless there (the submit always lands on
  // the same on/off state the click implies) and was the original intent of
  // this handler for the pre-checkbox mockup.
  document.addEventListener('click',function(e){var s=e.target.closest('.switch');if(s&&s.tagName!=='INPUT')s.classList.toggle('on');});

  // ---- live dashboard (Task 8: SSE + uPlot) ----
  // dashboard.html wraps its live content in <div id="dashboard-live">
  // (present only on that page, so this is a no-op everywhere else) with
  // fixed element IDs (#val-cpu, #chart-hero, ...) the tile/chart updates
  // below target. No inline handlers/scripts are used anywhere here -- the
  // strict CSP (security.go, script-src 'self' 'nonce-...') only ever
  // authorizes this external file.
  //
  // swBootSSE opens an EventSource against /events (viewer-gated exactly
  // like GET /, see routes.go) and, for each "snapshot" frame (JSON-encoded
  // web.DashboardView, sse.go), updates the tiles' text/led-class in place,
  // rebuilds the two "Top containers" hbar panels (renderContainerBars) from
  // the frame's top_cpu_containers/top_mem_containers, and appends the point
  // to a small client-side rolling buffer that feeds the three live uPlot
  // charts. There is no history backfill here -- full time-range charts
  // (querying the SampleStore) are Task 9; these charts only ever show
  // what's arrived over this SSE connection so far.
  window.swBootSSE=function(){
    if(!window.EventSource) return;
    var MAXPTS=120; // ~ a few minutes at a 5s fast tick
    var buf={t:[],cpu:[],load:[],mem:[],rx:[],tx:[]};
    var charts={};

    function setText(id,txt){var el=document.getElementById(id); if(el) el.textContent=txt;}
    function setLed(id,cls){var el=document.getElementById(id); if(el) el.className='led '+cls;}
    function led(v,warn,crit){return v>=crit?'crit':(v>=warn?'warn':'ok');}
    function loadLed(load1,cores){var c=cores>0?cores:1; return led(load1,0.7*c,c);}
    function humanRate(bps){
      if(bps>=1048576) return (bps/1048576).toFixed(1)+' MB/s';
      if(bps>=1024) return Math.round(bps/1024)+' KB/s';
      return Math.round(bps)+' B/s';
    }

    // renderContainerBars fills one of the "Top containers" hbar panels
    // (#top-cpu-bars/#top-mem-bars, dashboard.html) from an SSE frame's
    // top_cpu_containers/top_mem_containers list, mirroring handlers_
    // dashboard.go's containerBars: each row's bar width is normalized
    // against the list's OWN max value (not a fixed 0-100 scale -- MemMiB is
    // an absolute megabyte figure), and the whole panel is rebuilt from
    // scratch every frame rather than patched in place, so a container
    // list that grows/shrinks/reorders between frames doesn't leave stale
    // rows behind. list may be undefined/empty (collect.container_stats
    // disabled, or just not producing data yet) -- that renders the same
    // "no container stats available" placeholder the server-rendered page
    // shows, never a JS error. Built with createElement/textContent rather
    // than an innerHTML template string since a container name comes from
    // whatever docker reports (not sanitized like most of this file's other
    // innerHTML uses, e.g. renderDowntime's fixed enum/numeric fields).
    function renderContainerBars(id,list,rowClass,valueFn,formatFn){
      var box=document.getElementById(id);
      if(!box) return;
      box.innerHTML='';
      if(!list||!list.length){
        var note=document.createElement('div');
        note.className='note';
        note.textContent='no container stats available';
        box.appendChild(note);
        return;
      }
      var max=0;
      list.forEach(function(c){ var v=valueFn(c); if(v>max) max=v; });
      list.forEach(function(c){
        var v=valueFn(c);
        var width=max>0?(v/max*100):0;
        var row=document.createElement('div');
        row.className=rowClass?('hbar '+rowClass):'hbar';
        var lbl=document.createElement('span'); lbl.className='lbl'; lbl.textContent=c.name;
        var track=document.createElement('span'); track.className='track';
        var bar=document.createElement('i'); bar.style.width=width.toFixed(0)+'%';
        track.appendChild(bar);
        var val=document.createElement('span'); val.className='v'; val.textContent=formatFn(v);
        row.appendChild(lbl); row.appendChild(track); row.appendChild(val);
        box.appendChild(row);
      });
    }
    function containerCPUPct(c){ return c.cpu_pct; }
    function containerMemMiB(c){ return c.mem_mib; }
    function fmtContainerPct(v){ return Math.round(v)+'%'; }
    function fmtContainerMiB(v){ return Math.round(v)+'M'; }

    function ensureChart(id,series,colors){
      if(charts[id]) return charts[id];
      var el=document.getElementById(id);
      if(!el||!window.uPlot) return null;
      var uSeries=[{}];
      series.forEach(function(lbl,i){uSeries.push({label:lbl,stroke:colors[i],width:1.8,fill:colors[i]+'22'});});
      var opts={width:el.clientWidth||400,height:el.clientHeight||160,series:uSeries,cursor:{show:false},legend:{show:false},axes:swAxesOpt()};
      var data=[[]]; series.forEach(function(){data.push([]);});
      var u=new uPlot(opts,data,el);
      charts[id]=u;
      swRegisterChart(u,el);
      return u;
    }

    // rebuildCharts destroys every currently-mounted chart and immediately
    // re-renders (renderCharts, defined below) so ensureChart re-creates
    // each one with swAxesOpt()'s THEN-current theme colors -- the
    // 'sw-theme' listener below calls this after a theme toggle, since
    // uPlot has no public API to recolor an axis on an already-built
    // instance in place.
    function rebuildCharts(){
      Object.keys(charts).forEach(function(id){ charts[id].destroy(); delete charts[id]; });
      renderCharts();
    }

    function pushPoint(arr,v){arr.push(v); if(arr.length>MAXPTS) arr.shift();}

    function renderCharts(){
      var hero=ensureChart('chart-hero',['cpu %','load'],[CHART_COLORS.info,CHART_COLORS.pink]);
      if(hero) hero.setData([buf.t,buf.cpu,buf.load]);
      var mem=ensureChart('chart-mem',['mem %'],[CHART_COLORS.violet]);
      if(mem) mem.setData([buf.t,buf.mem]);
      var net=ensureChart('chart-net',['rx','tx'],[CHART_COLORS.cyan,CHART_COLORS.pink]);
      if(net) net.setData([buf.t,buf.rx,buf.tx]);
    }

    document.addEventListener('sw-theme',rebuildCharts);

    // Stale banner (task C6): sse.go's remoteNodeEventsLoop emits a "stale"
    // event after 3 consecutive poll failures on a remote node's dashboard,
    // naming the last-success time (Unix seconds); this shows a banner and
    // the next "snapshot" event (below) clears it -- a remote node's poll
    // always emits ONE snapshot frame on recovery even if the data is
    // unchanged, specifically so there's something to clear the banner on.
    var staleBanner=document.getElementById('stale-banner');
    function clearStaleBanner(){ if(staleBanner){ staleBanner.hidden=true; staleBanner.textContent=''; } }
    var es=new EventSource(nodeURL('/events'));
    es.addEventListener('stale',function(ev){
      if(!staleBanner) return;
      var s; try{ s=JSON.parse(ev.data); }catch(e){ s={}; }
      var when=s.last_success?new Date(s.last_success*1000).toLocaleTimeString():'unknown';
      staleBanner.textContent='live updates paused — last update '+when;
      staleBanner.hidden=false;
    });
    es.addEventListener('snapshot',function(ev){
      var s;
      try{ s=JSON.parse(ev.data); }catch(e){ return; }
      clearStaleBanner();

      setText('val-cpu',Math.round(s.cpu)+'%'); setLed('led-cpu',led(s.cpu,70,90));
      setText('val-mem',Math.round(s.mem_pct)+'%'); setLed('led-mem',led(s.mem_pct,75,90));
      setText('sub-mem-chart',Math.round(s.mem_pct)+'%');
      setText('val-swap',Math.round(s.swap_pct)+'%'); setLed('led-swap',led(s.swap_pct,40,80));
      setText('val-load1',s.load1.toFixed(2));
      setText('sub-load','5m '+s.load5.toFixed(2)+' · 15m '+s.load15.toFixed(2));
      setLed('led-load',loadLed(s.load1,s.cores));
      if(s.temp_c>0){ setText('val-temp',Math.round(s.temp_c)+'°C'); setLed('led-temp',led(s.temp_c,70,85)); }
      setText('val-net',humanRate(s.net_rx_bps)+'↓');
      setText('sub-net','↑'+humanRate(s.net_tx_bps));
      setText('sub-net-chart','↓'+humanRate(s.net_rx_bps)+' ↑'+humanRate(s.net_tx_bps));
      if(s.processes){
        setText('val-proc',String(s.processes.total));
        setText('sub-proc',s.processes.running+' run · '+s.processes.zombie+' zombie');
      }
      renderContainerBars('top-cpu-bars',s.top_cpu_containers,'',containerCPUPct,fmtContainerPct);
      renderContainerBars('top-mem-bars',s.top_mem_containers,'mem',containerMemMiB,fmtContainerMiB);

      pushPoint(buf.t,s.ts); pushPoint(buf.cpu,s.cpu); pushPoint(buf.load,s.load1);
      pushPoint(buf.mem,s.mem_pct); pushPoint(buf.rx,s.net_rx_bps); pushPoint(buf.tx,s.net_tx_bps);
      renderCharts();
    });
  };
  var liveRoot=document.getElementById('dashboard-live');
  if(liveRoot) window.swBootSSE();

  // ---- live public page (public-rework task: anonymous /public/events) ----
  // templates/public.html wraps its content in <div id="public-live">
  // (present only on that page). This is a SEPARATE, minimal boot function
  // from swBootSSE above -- it opens an EventSource against /public/events
  // (anonymous, allowlist-filtered server-side -- see sse.go's
  // publicEventsHandler/buildPublicSSEFrame) rather than /events, and its
  // "snapshot" frame is a small {panels:[{id,label,value,sub}],
  // availability?:{...}} object (publicSSEFrame, sse.go), not a full
  // DashboardView: there is nothing here to selectively render, because
  // there is nothing on the wire beyond what the server already decided to
  // expose. Both the tile grid and the availability strip are rebuilt from
  // scratch on every frame (rather than patched element-by-element like
  // swBootSSE's tiles) so a mid-connection admin edit to public.panels
  // (fewer/more panels, "availability" toggled off) is reflected exactly,
  // with no stale leftover element from a panel that's no longer allowed.
  window.swBootPublicSSE=function(){
    if(!window.EventSource) return;

    function renderPanels(list){
      var box=document.getElementById('pub-panels');
      if(!box) return;
      box.innerHTML='';
      if(!list||!list.length){
        var note=document.createElement('div');
        note.className='note';
        note.textContent='Nothing is published yet.';
        box.appendChild(note);
        return;
      }
      list.forEach(function(p){
        var tile=document.createElement('div');
        tile.className='tile';
        tile.setAttribute('data-panel',p.id);
        var k=document.createElement('div'); k.className='k';
        var eyebrow=document.createElement('span'); eyebrow.className='eyebrow'; eyebrow.textContent=p.label;
        k.appendChild(eyebrow); tile.appendChild(k);
        var val=document.createElement('div'); val.className='val'; val.setAttribute('data-field','value'); val.textContent=p.value;
        tile.appendChild(val);
        if(p.sub){
          var sub=document.createElement('div'); sub.className='sub'; sub.setAttribute('data-field','sub'); sub.textContent=p.sub;
          tile.appendChild(sub);
        }
        box.appendChild(tile);
      });
    }

    function renderAvailability(av){
      var panel=document.getElementById('pub-avail');
      if(!panel) return;
      if(!av){ panel.remove(); return; } // admin turned "availability" off mid-connection
      var summary=document.getElementById('pub-avail-summary');
      if(summary) summary.innerHTML='<span style="color:var(--ok)">'+av.uptime_pct.toFixed(2)+'% up</span> · '+av.incidents_label+' · '+av.downtime_str;
      var blocks=document.getElementById('pub-avail-blocks');
      if(blocks){
        blocks.innerHTML='';
        (av.blocks||[]).forEach(function(b){
          var seg=document.createElement('div');
          seg.className='seg'+(b.down?' down':'');
          seg.title=b.label+' · '+(b.down?'down':'up');
          blocks.appendChild(seg);
        });
      }
    }

    var es=new EventSource('/public/events');
    es.addEventListener('snapshot',function(ev){
      var s;
      try{ s=JSON.parse(ev.data); }catch(e){ return; }
      renderPanels(s.panels);
      renderAvailability(s.availability);
    });
  };
  var publicLiveRoot=document.getElementById('public-live');
  if(publicLiveRoot) window.swBootPublicSSE();

  // ---- history graphs (Task 9: /api/series + /api/downtime + uPlot) ----
  // templates/history.html wraps its charts in <div id="history-page"
  // data-history data-range="24h"> (present only on that page, so this is a
  // no-op everywhere else); the time-range chips (#historyRange,
  // data-range="1h|6h|24h|7d|30d") pick the [from,to] window the metric
  // charts query. No inline handlers/scripts anywhere here -- same strict CSP
  // as the rest of this file (security.go, script-src 'self' 'nonce-...').
  //
  // Each chart hook is one of:
  //   - <div data-metric="cpu">                 single-series (cpu/mem/temp)
  //   - <div data-metrics="load1,load5,load15"  multi-series: the mockup's
  //          data-labels="1m,5m,15m">           full load chart, and the
  //                                             per-mount disk-usage panel
  //          (data-metrics="disk:/,disk:/data", data-labels="/,/data"),
  //          whose mounts history.html resolves server-side from the live
  //          snapshot (handlers_history.go).
  // Every /api/series response is uPlot's own [ts[], avg[], min[], max[]]
  // parallel-array shape (handlers_history.go's seriesResponse), so a
  // single-series chart feeds it straight in; a multi-series chart fetches
  // each metric and merges them on a shared, sorted timestamp axis (gaps ->
  // null, which uPlot renders as a break).
  //
  // The "Downtime · 30d" panel (<div data-downtime>) is filled from
  // GET /api/downtime (downtimeResponse: {events:[{type,start,end,
  // duration_sec}]}) -- a proportional timeline SVG + one row per event,
  // matching the mockup's markup.
  var HISTORY_COLORS=[CHART_COLORS.pink,CHART_COLORS.info,CHART_COLORS.cyan,CHART_COLORS.violet];
  // HISTORY_METRIC_COLORS gives the single-series charts (cpu/mem/temp) a
  // color matching the mockup's palette instead of every one of them
  // defaulting to HISTORY_COLORS[0] (pink) -- CPU=info(blue),
  // Memory=violet, Temperature=pink. Metrics absent here (multi-series
  // charts don't consult this map) keep the existing HISTORY_COLORS cycling.
  var HISTORY_METRIC_COLORS={cpu:CHART_COLORS.info,mem:CHART_COLORS.violet,temp:CHART_COLORS.pink};
  var HISTORY_RANGE_SECONDS={'1h':3600,'6h':21600,'24h':86400,'7d':604800,'30d':2592000};

  function historyFmtDur(sec){
    sec=Math.max(0,Math.round(sec));
    var h=Math.floor(sec/3600), m=Math.floor((sec%3600)/60), s=sec%60;
    if(h>0) return h+'h '+m+'m';
    if(m>0) return m+'m';
    return s+'s';
  }
  function historyFmtTs(sec){
    var d=new Date(sec*1000);
    function p(n){return ('0'+n).slice(-2);}
    var mon=['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'][d.getMonth()];
    return mon+' '+d.getDate()+' '+p(d.getHours())+':'+p(d.getMinutes());
  }

  window.swBootHistoryCharts=function(){
    var root=document.querySelector('[data-history]');
    if(!root) return;

    var charts={};

    // ensureChart mounts (once) a uPlot with one line per label in labels,
    // colored from the optional colors array (falling back to HISTORY_COLORS
    // cycling per-index for any label without one -- the multi-series
    // charts, e.g. load1/5/15 or per-mount disk usage, don't pass colors at
    // all); subsequent calls reuse the instance.
    function ensureChart(el,labels,colors){
      if(charts[el.id]) return charts[el.id];
      if(!window.uPlot) return null;
      var series=[{}];
      labels.forEach(function(lbl,i){var c=(colors&&colors[i])||HISTORY_COLORS[i%HISTORY_COLORS.length]; series.push({label:lbl,stroke:c,width:1.8,fill:c+'22'});});
      var data=[[]]; labels.forEach(function(){data.push([]);});
      var opts={
        width:el.clientWidth||600,
        height:el.clientHeight||240,
        series:series,
        cursor:{show:true},
        legend:{show:false},
        axes:swAxesOpt()
      };
      var u=new uPlot(opts,data,el);
      charts[el.id]=u;
      swRegisterChart(u,el);
      return u;
    }

    // rebuildCharts mirrors swBootSSE's helper of the same name: destroy
    // every mounted chart and reload so ensureChart re-creates each with
    // swAxesOpt()'s then-current theme colors after a theme toggle.
    function rebuildCharts(){
      Object.keys(charts).forEach(function(id){ charts[id].destroy(); delete charts[id]; });
      loadAll();
    }

    function currentRange(){
      var span=HISTORY_RANGE_SECONDS[root.dataset.range]||HISTORY_RANGE_SECONDS['24h'];
      var to=Math.floor(Date.now()/1000);
      return {from:to-span, to:to};
    }

    function fetchSeries(metric,range){
      var url=nodeURL('/api/series?metric='+encodeURIComponent(metric)+'&from='+range.from+'&to='+range.to);
      return fetch(url,{credentials:'same-origin'})
        .then(function(r){ if(!r.ok) throw new Error('series fetch failed'); return r.json(); })
        .then(function(data){ return (data&&data.series&&data.series.length>=2)?data.series:[[],[]]; });
    }

    // mergeSeries aligns N single-metric responses (each [ts[],avg[],...])
    // onto one sorted union timestamp axis, so a multi-line chart shares an
    // x-axis even if the metrics' points don't line up 1:1. Returns uPlot
    // data: [unionTs, avg0AlignedToUnion, avg1..., ...] with null for a
    // timestamp a given metric has no sample at.
    function mergeSeries(responses){
      var tsSet={};
      responses.forEach(function(s){ (s[0]||[]).forEach(function(t){tsSet[t]=true;}); });
      var union=Object.keys(tsSet).map(Number).sort(function(a,b){return a-b;});
      var cols=[union];
      responses.forEach(function(s){
        var lookup={}, ts=s[0]||[], avg=s[1]||[];
        for(var i=0;i<ts.length;i++) lookup[ts[i]]=avg[i];
        cols.push(union.map(function(t){ return (t in lookup)?lookup[t]:null; }));
      });
      return cols;
    }

    function loadChart(el){
      var range=currentRange();
      var multi=el.dataset.metrics;
      if(multi){
        var metrics=multi.split(',').map(function(s){return s.trim();}).filter(Boolean);
        if(!metrics.length) return;
        var labels=(el.dataset.labels||multi).split(',').map(function(s){return s.trim();});
        Promise.all(metrics.map(function(m){return fetchSeries(m,range);}))
          .then(function(responses){
            var u=ensureChart(el,labels);
            if(u) u.setData(mergeSeries(responses));
            if(el.id==='chart-history-disk') renderDiskLegend(labels);
          })
          .catch(function(){});
      } else {
        var metric=el.dataset.metric;
        if(!metric) return;
        fetchSeries(metric,range)
          .then(function(series){
            var u=ensureChart(el,['avg'],[HISTORY_METRIC_COLORS[metric]]);
            if(u) u.setData([series[0],series[1]]);
          })
          .catch(function(){});
      }
    }

    function renderDiskLegend(labels){
      var box=document.getElementById('historyDiskLegend');
      if(!box) return;
      box.innerHTML=labels.map(function(lbl,i){
        return '<span><i style="background:'+HISTORY_COLORS[i%HISTORY_COLORS.length]+'"></i>'+lbl+'</span>';
      }).join('');
    }

    // renderDowntime fills the "Downtime · 30d" panel from /api/downtime: a
    // proportional timeline bar (green "up" base + a colored segment per
    // bucket with downtime) and one row per event, mirroring
    // ui-mockup/history.html -- now paged server-side (Task 117) instead of
    // fetching every event in the range up front:
    //   - The first request only asks for CAP events (limit=CAP,offset=0),
    //     exactly enough to fill the always-visible 8-row view, so a long
    //     history's /api/downtime payload stays small by default.
    //   - "Show all N incidents" fetches the rest of the range in one more
    //     request (bounded to at most 500 events, the server's max limit)
    //     and reveals everything loaded so far; if that still leaves more
    //     (a truly enormous history), the button relabels to "Show more" and
    //     repeats for the next chunk -- so no single request is ever
    //     unbounded, however large the history gets.
    //   - The "total Xs · YY.YY%" summary uses the response's total/
    //     total_seconds (computed server-side over the WHOLE range), not a
    //     sum over whatever's currently loaded, so it reads the same
    //     whether zero, one, or every page has been fetched.
    //   - The timeline bar is drawn once, from the FIRST response's
    //     `timeline` field (handlers_history.go's buildDowntimeTimeline: a
    //     fixed downtimeTimelineBuckets-length array of {start,end,
    //     down_seconds,type} buckets covering the whole range, computed
    //     server-side over every in-range event regardless of paging) --
    //     never from `loaded`, so the bar always shows the full 30-day
    //     picture even before "Show all"/"Show more" is clicked. Only the
    //     row LIST is paged; the bar isn't.
    function renderDowntime(){
      var panel=document.querySelector('[data-downtime]');
      if(!panel) return;
      var span=HISTORY_RANGE_SECONDS['30d'];
      var to=Math.floor(Date.now()/1000), from=to-span;
      var rows=document.getElementById('downtimeRows');
      var timeline=document.getElementById('downtimeTimeline');
      var summary=document.getElementById('downtimeSummary');

      var CAP=8, MAX_CHUNK=500;
      var loaded=[], total=0, totalSeconds=0, loading=false;

      function fetchPage(offset,limit){
        var url=nodeURL('/api/downtime?from='+from+'&to='+to+'&limit='+limit+'&offset='+offset);
        return fetch(url,{credentials:'same-origin'})
          .then(function(r){ if(!r.ok) throw new Error('downtime fetch failed'); return r.json(); });
      }

      // drawTimeline renders the bar from the server-computed bucket array
      // (one rect per bucket with any down_seconds -- an empty/all-zero
      // bucket leaves the base "up" bar showing through), not from any
      // paged event list. A <title> gives each colored bucket a basic
      // hover tooltip in place of the old per-incident segments.
      function drawTimeline(buckets){
        if(!timeline) return;
        if(!buckets||!buckets.length){ timeline.innerHTML=''; return; }
        var W=1200, bw=W/buckets.length, segs='';
        buckets.forEach(function(b,i){
          if(!b.down_seconds) return;
          var x=i*bw;
          var color=b.type==='power_down'?'var(--crit)':'var(--warn)';
          segs+='<rect x="'+x.toFixed(2)+'" y="14" width="'+Math.max(1,bw).toFixed(2)+'" height="16" fill="'+color+'">'+
            '<title>'+historyFmtDur(b.down_seconds)+' down in '+historyFmtTs(b.start)+' → '+historyFmtTs(b.end)+'</title></rect>';
        });
        timeline.innerHTML='<svg width="100%" height="46" viewBox="0 0 '+W+' 46" preserveAspectRatio="none">'+
          '<rect x="0" y="14" width="'+W+'" height="16" rx="3" fill="var(--ok)" opacity=".65"/>'+segs+'</svg>';
      }

      function drawSummary(){
        if(!summary) return;
        var pct=(100*(1-totalSeconds/span));
        summary.textContent='total '+historyFmtDur(totalSeconds)+' · '+pct.toFixed(2)+'%';
      }

      function rowHTML(e){
        var led=e.type==='power_down'?'crit':'warn';
        var dur=historyFmtDur(e.duration_sec||Math.max(0,(e.end-e.start)));
        return '<div class="row"><span class="led '+led+'"></span>'+
          '<div class="name">'+e.type+' · '+historyFmtTs(e.start)+' → '+historyFmtTs(e.end)+'</div>'+
          '<span class="mono note">'+dur+'</span></div>';
      }

      function paint(showAll){
        if(!rows) return;
        if(!total){
          rows.innerHTML='<div class="row"><span class="led ok"></span><div class="name"><b>No downtime recorded</b><div class="note">100% uptime over the last 30 days</div></div></div>';
          return;
        }
        var shown=showAll?loaded:loaded.slice(0,CAP);
        var html=shown.map(rowHTML).join('');
        if(!showAll && total>CAP){
          html+='<button type="button" class="btn ghost" id="dtMore" style="margin-top:8px">Show all '+total+' incidents</button>';
        } else if(showAll && loaded.length<total){
          html+='<button type="button" class="btn ghost" id="dtMore" style="margin-top:8px">Show more ('+(total-loaded.length)+' remaining)</button>';
        }
        rows.innerHTML=html;
        var more=document.getElementById('dtMore');
        if(more) more.addEventListener('click',loadMore);
      }

      // loadMore fetches the next chunk (bounded to MAX_CHUNK) starting
      // right after what's already loaded, appends it (loaded stays
      // newest-first since every page from the server already is), and
      // repaints fully expanded -- driving both "Show all" (the first
      // click, usually satisfied in one request) and "Show more" (a later
      // click, only shown once a single MAX_CHUNK-sized fetch still wasn't
      // the whole range). Doesn't touch the timeline bar -- it was already
      // complete from the first response.
      function loadMore(){
        if(loading || loaded.length>=total) return;
        loading=true;
        var limit=Math.min(total-loaded.length,MAX_CHUNK);
        fetchPage(loaded.length,limit)
          .then(function(data){
            loaded=loaded.concat((data&&data.events)||[]);
            loading=false;
            paint(true);
          })
          .catch(function(){
            loading=false;
            paint(true);
          });
      }

      fetchPage(0,CAP)
        .then(function(data){
          loaded=(data&&data.events)||[];
          total=(data&&data.total)||0;
          totalSeconds=(data&&data.total_seconds)||0;
          drawTimeline((data&&data.timeline)||[]);
          drawSummary();
          paint(false);
        })
        .catch(function(){
          if(rows) rows.innerHTML='<div class="note">could not load downtime</div>';
        });
    }

    var chartEls=root.querySelectorAll('[data-metric],[data-metrics]');
    function loadAll(){ chartEls.forEach(loadChart); }

    var rangeBar=document.getElementById('historyRange');
    if(rangeBar){
      rangeBar.addEventListener('click',function(e){
        var btn=e.target.closest('[data-range]');
        if(!btn||!rangeBar.contains(btn)) return;
        root.dataset.range=btn.dataset.range;
        loadAll();
      });
    }

    document.addEventListener('sw-theme',rebuildCharts);

    loadAll();
    renderDowntime();
  };
  var historyRoot=document.querySelector('[data-history]');
  if(historyRoot) window.swBootHistoryCharts();

  // ---- fleet compare (task C1b: /fleet's checkbox selection + /fleet/compare's chart) ----
  // templates/fleet.html's table rows each carry a ".fleet-compare-check"
  // checkbox (value=node id); #fleetCompareBtn/#fleetCompareCount live
  // outside the table's self-polling tbody (fleet_rows, hx-swap="outerHTML"
  // every 5s) so they survive a poll even though the checkboxes themselves
  // don't -- the listener is delegated onto #fleet-nodes (the <table>
  // itself, never replaced) rather than bound per-checkbox, so it keeps
  // working after every poll without re-binding anything.
  //
  // Round 1 review fix: htmx's every-5s outerHTML swap of #fleet-tbody
  // replaces every checkbox with a fresh, unchecked one, silently wiping
  // whatever an operator had selected mid-comparison. selectedNodeIds (a
  // Set, persisted across swaps) is the source of truth for "what's
  // selected" now, not the checkboxes' own DOM state: a change event
  // updates the Set, and an htmx:afterSwap re-applies the Set onto the
  // fresh checkboxes' .checked before refreshing the counter/button.
  //
  // /fleet/compare is master-local (routes.go, node_scope.go's
  // masterLocalPrefixes), never a per-node view, so this deliberately uses
  // a plain relative URL -- NOT nodeURL() -- exactly like the rest of
  // /fleet's own links.
  (function(){
    var table=document.getElementById('fleet-nodes');
    if(!table) return;
    var btn=document.getElementById('fleetCompareBtn');
    var count=document.getElementById('fleetCompareCount');
    var CAP=10;
    var selectedNodeIds=new Set();
    function checkboxes(){
      return table.querySelectorAll('.fleet-compare-check');
    }
    function refresh(){
      var ids=Array.from(selectedNodeIds);
      if(count) count.textContent=ids.length+' selected'+(ids.length>CAP?' (max '+CAP+')':'');
      if(btn) btn.disabled=ids.length===0||ids.length>CAP;
    }
    // reapplySelection re-checks every fresh checkbox htmx just swapped in
    // whose value is still in selectedNodeIds (a node removed from the
    // roster since simply drops out of the Set the next time it's touched
    // -- an id with no matching checkbox anywhere is harmless, just unused).
    function reapplySelection(){
      Array.prototype.forEach.call(checkboxes(),function(cb){
        cb.checked=selectedNodeIds.has(cb.value);
      });
    }
    table.addEventListener('change',function(e){
      if(!e.target||!e.target.classList||!e.target.classList.contains('fleet-compare-check')) return;
      if(e.target.checked) selectedNodeIds.add(e.target.value);
      else selectedNodeIds.delete(e.target.value);
      refresh();
    });
    document.body.addEventListener('htmx:afterSwap',function(e){
      if(!e.detail||!e.detail.target||e.detail.target.id!=='fleet-tbody') return;
      reapplySelection();
      refresh();
    });
    if(btn){
      btn.addEventListener('click',function(){
        var ids=Array.from(selectedNodeIds);
        if(!ids.length||ids.length>CAP) return;
        window.location.href='/fleet/compare?nodes='+ids.map(encodeURIComponent).join(',');
      });
    }
    refresh();
  })();

  // /fleet/compare's own chart: templates/fleet_compare.html embeds its
  // FleetSeries result once, server-side, as JSON in
  // #chart-fleet-compare[data-series] (handlers_fleet_compare.go's
  // fleetCompareData) -- no separate fetch, so the whole page (including
  // its one FleetSeries call) renders from a single request.
  window.swBootFleetCompare=function(){
    var el=document.getElementById('chart-fleet-compare');
    if(!el||!window.uPlot) return;
    var raw=el.dataset.series;
    if(!raw) return;
    var parsed;
    try{ parsed=JSON.parse(raw); }catch(e){ return; }
    var nodes=parsed.nodes||[];
    var chart=null;
    function build(){
      var uSeries=[{}];
      nodes.forEach(function(name,i){
        var c=HISTORY_COLORS[i%HISTORY_COLORS.length];
        uSeries.push({label:name,stroke:c,width:1.8,fill:c+'22'});
      });
      var opts={width:el.clientWidth||600,height:el.clientHeight||320,series:uSeries,cursor:{show:true},legend:{show:false},axes:swAxesOpt()};
      chart=new uPlot(opts,parsed.series||[[]],el);
      swRegisterChart(chart,el);
    }
    build();
    document.addEventListener('sw-theme',function(){
      if(chart) chart.destroy();
      build();
    });
  };
  var fleetCompareRoot=document.getElementById('chart-fleet-compare');
  if(fleetCompareRoot) window.swBootFleetCompare();

  // ---- passkey enrollment (templates/enroll.html) ----
  // navigator.credentials.create()'s PublicKeyCredentialCreationOptions (and
  // the credential it returns) carry several fields as ArrayBuffers, but the
  // wire format to/from the server (internal/web/auth_webauthn.go,
  // go-webauthn's protocol package) is base64url text throughout. These two
  // helpers are the only place that boundary is crossed.
  function b64urlToBuf(s){
    var pad=s.length%4===0?'':'='.repeat(4-(s.length%4));
    var bin=atob((s+pad).replace(/-/g,'+').replace(/_/g,'/'));
    var bytes=new Uint8Array(bin.length);
    for(var i=0;i<bin.length;i++) bytes[i]=bin.charCodeAt(i);
    return bytes.buffer;
  }
  function bufToB64url(buf){
    var bytes=new Uint8Array(buf), bin='';
    for(var i=0;i<bytes.byteLength;i++) bin+=String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
  }

  // enrollBeginSafeErrors (U8, 2026-09-25 UI audit fix): the exact,
  // plain-text response bodies POST /enroll/begin can fail with that are
  // safe to show verbatim -- enrollBeginHandler's own two http.Error calls
  // (routes.go) plus resolveEnrollRole/tokenStore.Redeem's errors
  // (enroll_tokens.go). Anything else (a webauthn-library error, a wrapped
  // filesystem error) falls back to the generic message below rather than
  // leaking internal detail to an unauthenticated caller.
  var enrollBeginSafeErrors=[
    'name is required',
    'an account with that name already exists; adding a passkey to an existing account will require an admin invite',
    'web: enrollment is closed; an admin-issued invite token is required',
    'web: enrollment token already used',
    'web: enrollment token expired',
    'web: unknown enrollment token'
  ];
  var enrollBtn=document.getElementById('enrollBtn');
  if(enrollBtn){
    enrollBtn.addEventListener('click',function(){
      var nameEl=document.getElementById('enrollName'), statusEl=document.getElementById('enrollStatus');
      var tokenEl=document.getElementById('enrollToken');
      var name=(nameEl&&nameEl.value||'').trim();
      var token=(tokenEl&&tokenEl.value||'').trim();
      if(!name){ if(statusEl) statusEl.textContent='Enter a name first.'; return; }
      if(statusEl) statusEl.textContent='Waiting for your device…';
      fetch('/enroll/begin',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:name,token:token})})
        .then(function(r){
          if(r.ok) return r.json();
          return r.text().then(function(t){
            t=(t||'').trim();
            throw new Error(enrollBeginSafeErrors.indexOf(t)>=0?t:'could not start enrollment');
          });
        })
        .then(function(opts){
          var pk=opts.publicKey;
          pk.challenge=b64urlToBuf(pk.challenge);
          pk.user.id=b64urlToBuf(pk.user.id);
          if(pk.excludeCredentials) pk.excludeCredentials.forEach(function(c){ c.id=b64urlToBuf(c.id); });
          return navigator.credentials.create({publicKey:pk});
        })
        .then(function(cred){
          return fetch('/enroll/finish',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:JSON.stringify({
            id:cred.id,
            rawId:bufToB64url(cred.rawId),
            type:cred.type,
            response:{
              clientDataJSON:bufToB64url(cred.response.clientDataJSON),
              attestationObject:bufToB64url(cred.response.attestationObject)
            }
          })});
        })
        .then(function(r){ if(!r.ok) throw new Error('could not finish enrollment'); if(statusEl) statusEl.textContent='Passkey created -- you can sign in now.'; })
        .catch(function(e){ if(statusEl) statusEl.textContent='Error: '+e.message; });
    });
  }

  // ---- passkey login (templates/login.html) ----
  // No username field -- the mockup's login page is a single "Continue with
  // passkey" button, so this uses navigator.credentials.get() against a
  // client-side discoverable ("resident key") credential: the authenticator
  // itself surfaces which stored passkey matches this site, and the server
  // resolves the account afterward from the assertion's userHandle
  // (internal/web/auth_webauthn.go's beginLogin/finishLogin).
  var loginBtn=document.getElementById('loginBtn');
  if(loginBtn){
    loginBtn.addEventListener('click',function(){
      var statusEl=document.getElementById('loginStatus');
      if(statusEl) statusEl.textContent='Waiting for your device…';
      fetch('/login/begin',{method:'POST',credentials:'same-origin'})
        .then(function(r){ if(!r.ok) throw new Error('could not start sign-in'); return r.json(); })
        .then(function(opts){
          var pk=opts.publicKey;
          pk.challenge=b64urlToBuf(pk.challenge);
          if(pk.allowCredentials) pk.allowCredentials.forEach(function(c){ c.id=b64urlToBuf(c.id); });
          return navigator.credentials.get({publicKey:pk});
        })
        .then(function(cred){
          return fetch('/login/finish',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json'},body:JSON.stringify({
            id:cred.id,
            rawId:bufToB64url(cred.rawId),
            type:cred.type,
            response:{
              clientDataJSON:bufToB64url(cred.response.clientDataJSON),
              authenticatorData:bufToB64url(cred.response.authenticatorData),
              signature:bufToB64url(cred.response.signature),
              userHandle:cred.response.userHandle?bufToB64url(cred.response.userHandle):null
            }
          })});
        })
        .then(function(r){ if(!r.ok) throw new Error('sign-in failed'); if(statusEl) statusEl.textContent='Signed in -- redirecting…'; window.location.assign('/'); })
        .catch(function(e){ if(statusEl) statusEl.textContent='Error: '+e.message; });
    });
  }

  // ---- quiet-hours preview (templates/config.html, Task 10/#66) ----
  // Ported from ui-mockup/config.html's inline <script> (the "redesigned
  // quiet-hours control" the task brief calls out to preserve), moved into
  // this external file because the strict CSP (security.go, script-src
  // 'self' 'nonce-...', no unsafe-inline) doesn't authorize inline <script>
  // blocks -- only this file and the one nonce'd boot tag in base.html.
  // Adapted from the mockup's <input type=time> (HH:MM) to hour-only
  // <select> elements: config.QuietHours only stores whole hours
  // ("H-H"/"HH-HH" -- see internal/config/config.go's validateQuietHours),
  // so minute-granularity in the UI would silently be lossy.
  (function(){
    var panel=document.getElementById('qh');
    if(!panel) return;
    var on=document.getElementById('qhOn'), from=document.getElementById('qhFrom'), to=document.getElementById('qhTo'),
        bar=document.getElementById('qhBar'), sum=document.getElementById('qhSummary');
    function h12(h){var ap=h<12?'am':'pm',hh=h%12;if(hh===0)hh=12;return hh+':00'+ap;}
    function seg(a,b){var d=document.createElement('div');d.className='mute';d.style.left=(a/1440*100)+'%';d.style.width=((b-a)/1440*100)+'%';bar.appendChild(d);}
    function render(){
      bar.innerHTML='';
      var enabled=on.checked;
      panel.classList.toggle('qh-off',!enabled);
      if(enabled){
        var a=(+from.value)*60, b=(+to.value)*60;
        if(a===b){ seg(0,1440); }
        else if(a<b){ seg(a,b); }
        else { seg(a,1440); seg(0,b); } // wraps midnight
      }
      var n=document.createElement('div'); n.className='now';
      var now=new Date(), nm=now.getHours()*60+now.getMinutes();
      n.style.left=(nm/1440*100)+'%'; bar.appendChild(n);
      if(!enabled){ sum.textContent='off -- alerts any time'; sum.className='badge'; }
      else {
        var fh=+from.value, th=+to.value, dur=((th-fh+24)%24)||24;
        var wraps=fh>th;
        sum.textContent='quiet '+h12(fh)+' → '+h12(th)+(wraps?' (next day)':'')+' · '+dur+'h';
        sum.className='badge warn';
      }
    }
    on.addEventListener('change',render);
    from.addEventListener('change',render); to.addEventListener('change',render);
    render();
  })();

  // ---- logout (base.html: topbar sign-out button + mobile "More" sheet) ----
  // Both the desktop sidebar footer (#logoutBtn) and the mobile More sheet
  // (#logoutBtnSheet) carry a sign-out control; wire whichever exist to the
  // same POST /logout flow.
  function swLogout(){
    var meta=document.querySelector('meta[name="csrf-token"]');
    var csrf=meta?meta.content:'';
    fetch('/logout',{method:'POST',credentials:'same-origin',headers:{'X-CSRF-Token':csrf}})
      .then(function(){ window.location.assign('/login'); })
      .catch(function(){ window.location.assign('/login'); });
  }
  ['logoutBtn','logoutBtnSheet'].forEach(function(id){
    var b=document.getElementById(id);
    if(b) b.addEventListener('click',swLogout);
  });

  // ---- node switcher + Ctrl/Cmd-K palette (Task 6: fleet-web-a) -----------
  // Both are master-only chrome: base.html only renders #nodeSwitcher/
  // #nodePalette at all when PageData.Switcher is set (this daemon is a
  // fleet master) -- solo/child pages have neither element in the DOM, so
  // every lookup below is null and the whole block is inert there.
  (function(){
    var switcherRoot=document.getElementById('nodeSwitcher');
    var switcherBtn=document.getElementById('nodeSwitcherBtn');
    var switcherMenu=document.getElementById('nodeSwitcherList');
    var palette=document.getElementById('nodePalette');
    if(!switcherRoot && !palette) return; // solo/child: neither renders

    // ---- dropdown switcher ----
    function closeSwitcher(){
      if(!switcherMenu||switcherMenu.hidden) return;
      switcherMenu.hidden=true;
      switcherBtn.setAttribute('aria-expanded','false');
    }
    function openSwitcher(){
      if(!switcherMenu||!switcherMenu.hidden) return;
      switcherMenu.hidden=false;
      switcherBtn.setAttribute('aria-expanded','true');
      var first=switcherMenu.querySelector('.ns-item');
      if(first) first.focus();
    }
    if(switcherBtn && switcherMenu){
      switcherBtn.addEventListener('click',function(){
        if(switcherMenu.hidden) openSwitcher(); else closeSwitcher();
      });
      document.addEventListener('click',function(e){
        if(!switcherRoot.contains(e.target)) closeSwitcher();
      });
      switcherMenu.addEventListener('keydown',function(e){
        var items=Array.prototype.slice.call(switcherMenu.querySelectorAll('.ns-item,.ns-viewall'));
        var i=items.indexOf(document.activeElement);
        if(e.key==='ArrowDown'){ e.preventDefault(); if(items.length) items[(i+1+items.length)%items.length].focus(); }
        else if(e.key==='ArrowUp'){ e.preventDefault(); if(items.length) items[(i-1+items.length)%items.length].focus(); }
        else if(e.key==='Escape'){ e.preventDefault(); closeSwitcher(); switcherBtn.focus(); }
        else if(e.key==='Tab'){ closeSwitcher(); } // let focus leave normally, just close first
      });
      document.addEventListener('keydown',function(e){
        if(e.key==='Escape' && switcherMenu && !switcherMenu.hidden){ closeSwitcher(); switcherBtn.focus(); }
      });
    }

    // ---- Ctrl/Cmd-K palette ----
    if(!palette) return;
    var scrim=document.getElementById('nodePaletteScrim');
    var input=document.getElementById('nodePaletteInput');
    var results=document.getElementById('nodePaletteResults');
    var viewAllLink=document.getElementById('nodePaletteViewAll');
    var lastFocused=null;
    var cache=null, cacheAt=0;
    var CACHE_MS=30000; // the ruling: cache the fetched roster for 30s

    // ---- recent nodes (task C6): last 5 visited, in localStorage ----------
    // Wrapped in try/catch throughout (the ruling): a private-browsing tab,
    // a full storage quota, or a blocked site-data setting can all make
    // localStorage throw on read OR write, and none of that should ever
    // break the palette -- it just falls back to "no recent nodes" silently.
    var RECENT_KEY='sw-recent-nodes', RECENT_MAX=5;
    function loadRecentNodes(){
      try{
        var raw=window.localStorage.getItem(RECENT_KEY);
        var arr=raw?JSON.parse(raw):[];
        return Array.isArray(arr)?arr:[];
      }catch(e){ return []; }
    }
    // recordRecentNode pushes id to the front of the recent list (moving it
    // there if already present, so the list is always MRU-first), dropping
    // anything past RECENT_MAX. "self" is never recorded -- it's always
    // reachable as the switcher/palette's own fixed first-class entry, not
    // something a user needs "recent" tracking to find again.
    function recordRecentNode(id){
      if(!id || id==='self') return;
      try{
        var arr=loadRecentNodes().filter(function(x){ return x!==id; });
        arr.unshift(id);
        window.localStorage.setItem(RECENT_KEY, JSON.stringify(arr.slice(0,RECENT_MAX)));
      }catch(e){ /* best-effort only, see this block's doc */ }
    }
    // Record THIS page's own node as visited, once per page load -- every
    // node-scoped (or self) page a signed-in master sees this chrome on
    // counts as a "visit", not just ones reached through the switcher/
    // palette themselves.
    recordRecentNode(currentNodeID());
    function currentNodeID(){
      var prefix=(document.body && document.body.dataset.nodePrefix)||'';
      return prefix.indexOf('/n/')===0?prefix.slice(3):'self';
    }

    // ---- page-type jump (task C6): "web1 history" -> /n/web1/history ------
    // The fixed, closed set of page-type words a query can end with --
    // matching node_scope.go's node-routable pages this switcher/palette
    // otherwise targets via currentTargetPath() (the CURRENT page's own
    // type, preserved across a plain node switch).
    var PAGE_TYPES=['dashboard','monitoring','host','alerts','history'];
    function pageTypePath(t){ return t==='dashboard'?'/':'/'+t; }
    // parsePaletteQuery splits raw input into {query, pageType}: pageType is
    // set only when there's a name/tag query BEFORE the trailing page-type
    // word (so a bare "history" with nothing before it is treated as an
    // ordinary substring query against node names/tags, not a jump -- the
    // ruling's own example is "web1 history", name first).
    function parsePaletteQuery(raw){
      var trimmed=(raw||'').trim();
      if(!trimmed) return {query:'',pageType:null};
      var parts=trimmed.split(/\s+/);
      var last=parts[parts.length-1].toLowerCase();
      if(parts.length>1 && PAGE_TYPES.indexOf(last)>-1){
        return {query:parts.slice(0,-1).join(' ').toLowerCase(), pageType:last};
      }
      return {query:trimmed.toLowerCase(), pageType:null};
    }

    // MASTER_LOCAL mirrors node_scope.go's masterLocalPrefixes: a page under
    // one of these has no per-node counterpart, so a palette result targets
    // that node's dashboard ("/") instead of a (nonexistent) node-scoped
    // copy of the current page -- templates.go's switcherTargetPath is the
    // server-side source of truth for the same rule (used by the topbar
    // switcher above, computed once at render time); this is a client-side
    // duplicate purely because the palette computes its link target in the
    // browser rather than round-tripping to the server for it. Keep both in
    // sync if masterLocalPrefixes ever changes.
    var MASTER_LOCAL=['/config','/channels','/users','/settings','/enroll','/login','/logout','/assets','/public','/fleet'];
    function isMasterLocal(p){
      return MASTER_LOCAL.some(function(prefix){ return p===prefix || p.indexOf(prefix+'/')===0; });
    }
    function currentTargetPath(){
      var prefix=(document.body && document.body.dataset.nodePrefix)||'';
      var p=window.location.pathname;
      if(prefix && p.indexOf(prefix)===0) p=p.slice(prefix.length)||'/';
      if(isMasterLocal(p)) return '/'; // no query string -- see MASTER_LOCAL's doc
      // Round-1 review: keep the current query string when switching (e.g.
      // /history?metric=cpu -> /n/web1/history?metric=cpu), mirroring
      // templates.go's switcherTargetPath (server-rendered switcher).
      return p+(window.location.search||'');
    }
    // core.SelfNodeID's wire value is the literal "self" -- every
    // /api/fleet/nodes entry for this daemon's own node carries id:"self".
    // pageType (task C6), when set, overrides currentTargetPath() with a
    // fixed node-routable page instead of "the page I'm currently on" --
    // see parsePaletteQuery's doc.
    function hrefFor(node,pageType){
      var prefix=(node.id==='self')?'':('/n/'+node.id);
      var path=pageType?pageTypePath(pageType):currentTargetPath();
      return prefix+path;
    }
    function ledClassFor(state){
      if(state==='online') return 'ok';
      if(state==='down') return 'crit';
      if(state==='lagging'||state==='stale') return 'warn';
      return '';
    }

    function loadNodes(){
      var now=Date.now();
      if(cache && (now-cacheAt)<CACHE_MS) return Promise.resolve(cache);
      // Deliberately NOT nodeURL(...): /api/fleet/nodes is the master's own
      // fleet roster, a master-local endpoint (node_scope.go's
      // masterLocalPrefixes lists "/fleet") with no per-node counterpart to
      // route to -- the same reason /fleet's own links are never
      // nodeURL(...)-wrapped either. Exempted by name from
      // TestAppJSDataFetchesGoThroughNodeURL's scan (templates_node_test.go)
      // for exactly this reason.
      return fetch('/api/fleet/nodes',{credentials:'same-origin'})
        .then(function(r){ if(!r.ok) throw new Error('HTTP '+r.status); return r.json(); })
        .then(function(nodes){ cache=nodes||[]; cacheAt=Date.now(); return cache; })
        .catch(function(){ return cache||[]; });
    }
    // matchesQuery is a plain case-insensitive SUBSTRING match (round-1
    // review: task-6-brief.md's wording is "fuzzy match", but this
    // implements a literal substring match instead -- the same convention
    // /fleet's own ?q= filter uses server-side, fleetFilterMatch in
    // handlers_fleet.go -- not fuzzy/ranked matching. Confirmed intentional
    // by round-1 review ("leave the substring matching as it is").
    function matchesQuery(n,q){
      if(!q) return true;
      // remote_addr is present in the JSON only for an admin session
      // (fleetNodesAPIHandler blanks it for a viewer before encoding), so
      // this naturally matches address only when the caller can see it --
      // no role check needed here.
      var hay=((n.name||'')+' '+(n.tags||[]).join(' ')+' '+(n.remote_addr||'')).toLowerCase();
      return hay.indexOf(q)>-1;
    }
    function renderResults(nodes){
      var parsed=parsePaletteQuery(input.value);
      var filtered=(nodes||[]).filter(function(n){ return matchesQuery(n,parsed.query); });
      // Recent-first (task C6): with no name/tag query typed yet, surface
      // the last-visited nodes at the top rather than whatever order
      // /api/fleet/nodes happened to return them in -- a node not in the
      // recent list keeps its relative order, stably sorted to the end.
      if(!parsed.query){
        var recent=loadRecentNodes();
        if(recent.length){
          filtered=filtered.slice().sort(function(a,b){
            var ai=recent.indexOf(a.id), bi=recent.indexOf(b.id);
            if(ai===-1) ai=recent.length;
            if(bi===-1) bi=recent.length;
            return ai-bi;
          });
        }
      }
      results.innerHTML='';
      if(!filtered.length){
        results.appendChild(mkEl('div','note node-palette-empty','No nodes match.'));
        return;
      }
      filtered.forEach(function(n){
        var row=mkEl('a','ns-item');
        row.href=hrefFor(n,parsed.pageType);
        row.setAttribute('role','option');
        row.appendChild(mkEl('span','led '+ledClassFor(n.state)));
        row.appendChild(mkEl('span','ns-name',n.id==='self'?'this server':(n.name||n.id)));
        row.appendChild(mkEl('span','ns-state note',n.state||'online'));
        results.appendChild(row);
      });
    }

    function openPalette(){
      closeSwitcher(); // round-1 review: don't leave two Escape handlers both live
      lastFocused=document.activeElement;
      palette.hidden=false;
      input.value='';
      results.innerHTML='';
      loadNodes().then(renderResults);
      input.focus();
    }
    // paletteFocusable is the palette's full Tab-trap cycle, in DOM/visual
    // order: the search input, every currently-rendered result, and --
    // round-1 review -- the footer's "View all in Fleet" link as the TRUE
    // last element (previously left out of the trap: Tab from the last
    // .ns-item wrapped straight back to the input, so the footer link was
    // never keyboard-reachable at all in an aria-modal dialog, where every
    // focusable element must be). viewAllLink is always present in the
    // static markup (base.html), so this array always has >=2 entries.
    function paletteFocusable(){
      var items=Array.prototype.slice.call(results.querySelectorAll('.ns-item'));
      var all=[input].concat(items);
      if(viewAllLink) all.push(viewAllLink);
      return all;
    }
    function closePalette(){
      if(palette.hidden) return;
      palette.hidden=true;
      if(lastFocused && lastFocused.focus) lastFocused.focus();
      lastFocused=null;
    }

    input.addEventListener('input',function(){ loadNodes().then(renderResults); });
    input.addEventListener('keydown',function(e){
      if(e.key==='ArrowDown'){ e.preventDefault(); var first=results.querySelector('.ns-item'); if(first) first.focus(); }
      else if(e.key==='Enter'){ var first=results.querySelector('.ns-item'); if(first) window.location.assign(first.getAttribute('href')); }
    });
    results.addEventListener('keydown',function(e){
      var items=Array.prototype.slice.call(results.querySelectorAll('.ns-item'));
      var i=items.indexOf(document.activeElement);
      if(e.key==='ArrowDown'){ e.preventDefault(); if(items.length) (items[i+1]||items[0]).focus(); }
      else if(e.key==='ArrowUp'){ e.preventDefault(); if(i<=0){ input.focus(); } else { items[i-1].focus(); } }
      else if(e.key==='Enter'){ var el=document.activeElement; if(el&&el.classList.contains('ns-item')) el.click(); }
    });
    palette.addEventListener('keydown',function(e){
      if(e.key==='Escape'){ e.preventDefault(); closePalette(); return; }
      if(e.key!=='Tab') return;
      // Trap focus within the palette while it's open (paletteFocusable's
      // doc: the footer link is the true last element in the cycle -- Tab
      // from it wraps to the input, Shift+Tab from the input wraps to it).
      var focusable=paletteFocusable();
      var firstEl=focusable[0], lastEl=focusable[focusable.length-1];
      if(e.shiftKey && document.activeElement===firstEl){ e.preventDefault(); lastEl.focus(); }
      else if(!e.shiftKey && document.activeElement===lastEl){ e.preventDefault(); firstEl.focus(); }
    });
    if(scrim) scrim.addEventListener('click',closePalette);

    document.addEventListener('keydown',function(e){
      var mod=e.metaKey||e.ctrlKey;
      if(!mod || e.key.toLowerCase()!=='k') return;
      if(!palette.hidden){ e.preventDefault(); closePalette(); return; }
      var el=document.activeElement;
      var typing=(el && /^(input|textarea|select)$/i.test(el.tagName)) || (el && el.isContentEditable);
      if(typing) return; // ruling: not while focus is in an input/textarea/select/contenteditable
      e.preventDefault();
      openPalette();
    });
  })();

  // ---- mobile "More" sheet (base.html bottom-nav) ----
  // The bottom tab bar's "More" button opens a slide-up sheet listing the
  // secondary nav items + account/sign-out. Dependency-free: toggle an `.on`
  // class the CSS (@media max-width:640px) animates; prefers-reduced-motion is
  // honored purely in CSS (transition:none). The sheet/scrim live in the DOM
  // on every page but are display:none above 640px, so this is inert on desktop.
  (function(){
    var moreBtn=document.getElementById('moreBtn'),
        sheet=document.getElementById('moreSheet'),
        scrim=document.getElementById('moreScrim');
    if(!moreBtn||!sheet||!scrim) return;
    function open(){ sheet.classList.add('on'); scrim.classList.add('on'); moreBtn.setAttribute('aria-expanded','true'); }
    function close(){ sheet.classList.remove('on'); scrim.classList.remove('on'); moreBtn.setAttribute('aria-expanded','false'); }
    function toggle(){ (sheet.classList.contains('on')?close:open)(); }
    moreBtn.addEventListener('click',toggle);
    scrim.addEventListener('click',close);
    // a tap on any nav link inside the sheet navigates away -- close first so
    // it isn't left open behind the next page's paint on a client-cached back.
    sheet.addEventListener('click',function(e){ if(e.target.closest('a')) close(); });
    document.addEventListener('keydown',function(e){ if(e.key==='Escape') close(); });
  })();
})();
