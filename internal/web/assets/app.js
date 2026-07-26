/* serverwatch web UI — shared client behavior.
 *
 * Ported from ui-mockup/assets/app.js. The mockup injected the sidebar/topbar
 * shell client-side (from a NAV array + data-shell attributes) and demoed
 * role-switching via a `.mockbar` + localStorage; the shell is now rendered
 * server-side (see internal/web/templates/base.html, the "nav" block) with
 * the real role coming from the session (stubbed to "admin" until Task
 * 5/6's auth lands), so all of that is stripped here. Kept: theme toggle,
 * the SVG gradient defs, the heartbeat/availability-strip/sparkline
 * helpers, and the drawer/modal/tabs/filter/switch interaction handlers —
 * later tasks (dashboard/monitoring/history/channels pages) reuse these
 * once they render real markup into the same hooks.
 */
(function(){
  // ---- gradient defs ----
  document.body.insertAdjacentHTML('afterbegin','<svg width="0" height="0" style="position:absolute" aria-hidden="true"><defs>'+
    ['gInfo','gViolet','gOk','gCrit','gCyan','gSig'].map(function(id){return '<linearGradient id="'+id+'" x1="0" y1="0" x2="0" y2="1"><stop class="a" offset="0"/><stop class="b" offset="1"/></linearGradient>';}).join('')+'</defs></svg>');

  // ---- theme ----
  function setTheme(t){document.documentElement.dataset.theme=t; try{localStorage.sw_theme=t}catch(e){}}
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

  // ---- availability strip ----
  var strip=document.getElementById('hbstrip');
  if(strip){
    var N=96,mins=15,dF=68,dT=70,wA=41,now=new Date(),segs='',up=0,i;
    for(i=0;i<N;i++){var ago=(N-1-i)*mins,tt=new Date(now.getTime()-ago*60000),hh=('0'+tt.getHours()).slice(-2)+':'+('0'+tt.getMinutes()).slice(-2),cls='seg',st='up';
      if(i>=dF&&i<=dT){cls+=' down';st='DOWN';}else if(i===wA){cls+=' warn';st='degraded';}else up++;
      segs+='<div class="'+cls+'" title="'+hh+' · '+st+'"></div>';}
    strip.innerHTML='<div class="head" style="margin-bottom:10px"><span class="eyebrow">Availability · 24h · '+mins+'-min blocks</span><span class="mono small"><span style="color:var(--ok)">'+(up/N*100).toFixed(2)+'% up</span> · 1 incident · 45m</span></div>'+
      '<div class="avail">'+segs+'</div>'+
      '<div class="note mono" style="display:flex;justify-content:space-between;margin-top:8px"><span>24h ago</span><span>18h</span><span>12h</span><span>6h</span><span>now</span></div>'+
      '<div class="legend" style="margin-top:10px"><span><i style="background:var(--ok)"></i>up</span><span><i style="background:var(--warn)"></i>degraded</span><span><i style="background:var(--crit)"></i>down</span><span class="note">hover a block for the time</span></div>';
  }

  // ---- uptime bars (public) ----
  var upb=document.getElementById('upbars');
  if(upb){var h='',k;for(k=0;k<90;k++){var c='var(--ok)',ht=38;if(k===69){c='var(--crit)'}else if(k===72){c='var(--warn)';ht=30}h+='<div style="flex:1;height:'+ht+'px;background:'+c+';opacity:.85;border-radius:2px"></div>';}upb.innerHTML=h;}

  // ---- detail drawer ----
  document.body.insertAdjacentHTML('beforeend','<div class="drawer-scrim" id="swScrim"></div><aside class="drawer" id="swDrawer"></aside>');
  var drawer=document.getElementById('swDrawer'), dScrim=document.getElementById('swScrim');
  function closeDrawer(){drawer.classList.remove('on');dScrim.classList.remove('on');}
  dScrim.addEventListener('click',closeDrawer);
  document.addEventListener('keydown',function(e){if(e.key==='Escape')closeDrawer();});
  function spark(color){return '<svg width="100%" height="60" viewBox="0 0 300 60" preserveAspectRatio="none"><path fill="url(#g'+color.g+')" class="ln" stroke="var(--'+color.c+')" stroke-width="1.6" d="M0,44 40,38 80,46 120,24 160,40 200,16 240,34 280,26 300,30 L300,60 0,60Z"/></svg>';}
  document.addEventListener('click',function(e){
    var row=e.target.closest('[data-detail]'); if(!row) return;
    var d=row.dataset, kind=d.kind||'item';
    var role=document.body.dataset.role||'admin';
    var stateBadge=d.state?'<span class="badge '+(d.state==='running'||d.state==='active'||d.state==='PASS'?'ok':(d.state==='restarting'?'warn':'crit'))+'">'+d.state+'</span>':'';
    var info='';
    Object.keys(d).forEach(function(k){ if(['detail','name','kind','state'].indexOf(k)>-1)return; info+='<dt>'+k+'</dt><dd>'+d[k]+'</dd>'; });
    var charts = (kind==='container'||kind==='process') ?
      '<div class="section-label"><span class="eyebrow">CPU · 1h</span></div><div class="panel" style="padding:10px">'+spark({g:'Info',c:'info'})+'</div>'+
      '<div class="section-label"><span class="eyebrow">Memory · 1h</span></div><div class="panel" style="padding:10px">'+spark({g:'Violet',c:'violet'})+'</div>'
      : (kind==='disk' ?
      '<div class="section-label"><span class="eyebrow">Usage · 30d + projection</span></div><div class="panel" style="padding:10px">'+spark({g:'Crit',c:'crit'})+'</div>'
      : '<div class="section-label"><span class="eyebrow">Memory · 1h</span></div><div class="panel" style="padding:10px">'+spark({g:'Ok',c:'ok'})+'</div>');
    var actions = role==='admin' ? (kind==='container'
      ? '<button class="btn">Restart</button><button class="btn ghost">View logs</button><button class="btn ghost">Pause monitoring</button>'
      : '<button class="btn ghost">Pause monitoring</button>') : '<span class="note">viewer — read-only</span>';
    drawer.innerHTML='<div class="dh"><div style="flex:1"><div class="eyebrow">'+kind+'</div><h3 style="margin:2px 0 0;font-size:16px">'+(d.name||'')+'</h3></div>'+stateBadge+'<button class="icon-btn" id="swClose">✕</button></div>'+
      '<div class="db"><dl class="kv">'+info+'</dl>'+charts+
      '<div class="section-label"><span class="eyebrow">Actions</span></div><div style="display:flex;gap:8px;flex-wrap:wrap">'+actions+'</div></div>';
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

  // ---- tabs / filter / chips / switches ----
  document.addEventListener('click',function(e){var b=e.target.closest('.tabs button');if(!b)return;var w=b.closest('[data-tabs]');w.querySelectorAll('.tabs button').forEach(function(x){x.classList.toggle('on',x===b)});w.querySelectorAll('.tabpane').forEach(function(p){p.classList.toggle('on',p.dataset.pane===b.dataset.tab)});});
  document.querySelectorAll('[data-filter]').forEach(function(inp){inp.addEventListener('input',function(){var q=inp.value.toLowerCase();document.querySelectorAll(inp.dataset.filter).forEach(function(tbl){tbl.querySelectorAll('tbody tr').forEach(function(tr){tr.style.display=tr.textContent.toLowerCase().indexOf(q)>-1?'':'none';});});});});
  document.addEventListener('click',function(e){var c=e.target.closest('.chip');if(c&&c.parentElement&&c.parentElement.classList.contains('chips')){c.parentElement.querySelectorAll('.chip').forEach(function(x){x.classList.remove('on')});c.classList.add('on');}});
  document.addEventListener('click',function(e){var s=e.target.closest('.switch');if(s)s.classList.toggle('on');});

  // ---- live dashboard (Task 8: SSE + uPlot) ----
  // dashboard.html wraps its live content in <div id="dashboard-live">
  // (present only on that page, so this is a no-op everywhere else) with
  // fixed element IDs (#val-cpu, #chart-hero, ...) the tile/chart updates
  // below target. No inline handlers/scripts are used anywhere here — the
  // strict CSP (security.go, script-src 'self' 'nonce-...') only ever
  // authorizes this external file.
  //
  // swBootSSE opens an EventSource against /events (viewer-gated exactly
  // like GET /, see routes.go) and, for each "snapshot" frame (JSON-encoded
  // web.DashboardView, sse.go), updates the tiles' text/led-class in place
  // and appends the point to a small client-side rolling buffer that feeds
  // the three live uPlot charts. There is no history backfill here — full
  // time-range charts (querying the SampleStore) are Task 9; these charts
  // only ever show what's arrived over this SSE connection so far.
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

    function ensureChart(id,series,colors){
      if(charts[id]) return charts[id];
      var el=document.getElementById(id);
      if(!el||!window.uPlot) return null;
      var uSeries=[{}];
      series.forEach(function(lbl,i){uSeries.push({label:lbl,stroke:colors[i],width:1.5});});
      var opts={width:el.clientWidth||400,height:el.clientHeight||160,series:uSeries,cursor:{show:false},legend:{show:false},axes:[{},{}]};
      var data=[[]]; series.forEach(function(){data.push([]);});
      var u=new uPlot(opts,data,el);
      charts[id]=u;
      return u;
    }

    function pushPoint(arr,v){arr.push(v); if(arr.length>MAXPTS) arr.shift();}

    function renderCharts(){
      var hero=ensureChart('chart-hero',['cpu %','load'],['var(--info)','var(--signal)']);
      if(hero) hero.setData([buf.t,buf.cpu,buf.load]);
      var mem=ensureChart('chart-mem',['mem %'],['var(--violet)']);
      if(mem) mem.setData([buf.t,buf.mem]);
      var net=ensureChart('chart-net',['rx','tx'],['var(--cyan)','var(--signal)']);
      if(net) net.setData([buf.t,buf.rx,buf.tx]);
    }

    var es=new EventSource('/events');
    es.addEventListener('snapshot',function(ev){
      var s;
      try{ s=JSON.parse(ev.data); }catch(e){ return; }

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

      pushPoint(buf.t,s.ts); pushPoint(buf.cpu,s.cpu); pushPoint(buf.load,s.load1);
      pushPoint(buf.mem,s.mem_pct); pushPoint(buf.rx,s.net_rx_bps); pushPoint(buf.tx,s.net_tx_bps);
      renderCharts();
    });
  };
  var liveRoot=document.getElementById('dashboard-live');
  if(liveRoot) window.swBootSSE();

  // Task 9 wires uPlot (already embedded, see assets/uPlot.iife.min.js +
  // uPlot.min.css) against SampleStore history queries for the history page.
  window.swBootHistoryCharts=function(){};

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
        .then(function(r){ if(!r.ok) throw new Error('could not start enrollment'); return r.json(); })
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
        .then(function(r){ if(!r.ok) throw new Error('could not finish enrollment'); if(statusEl) statusEl.textContent='Passkey created — you can sign in now.'; })
        .catch(function(e){ if(statusEl) statusEl.textContent='Error: '+e.message; });
    });
  }

  // ---- passkey login (templates/login.html) ----
  // No username field — the mockup's login page is a single "Continue with
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
        .then(function(r){ if(!r.ok) throw new Error('sign-in failed'); if(statusEl) statusEl.textContent='Signed in — redirecting…'; window.location.assign('/'); })
        .catch(function(e){ if(statusEl) statusEl.textContent='Error: '+e.message; });
    });
  }

  // ---- logout (base.html topbar sign-out button) ----
  var logoutBtn=document.getElementById('logoutBtn');
  if(logoutBtn){
    logoutBtn.addEventListener('click',function(){
      var meta=document.querySelector('meta[name="csrf-token"]');
      var csrf=meta?meta.content:'';
      fetch('/logout',{method:'POST',credentials:'same-origin',headers:{'X-CSRF-Token':csrf}})
        .then(function(){ window.location.assign('/login'); })
        .catch(function(){ window.location.assign('/login'); });
    });
  }
})();
