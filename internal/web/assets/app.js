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

  // ---- live data boot (stubs; filled in by Task 8 (SSE) / Task 9 (uPlot)) ----
  // Task 8 wires an EventSource against /events here to push live snapshot
  // updates into the dashboard/monitoring tiles without a page reload.
  window.swBootSSE=function(){};
  // Task 9 wires uPlot (already embedded, see assets/uPlot.iife.min.js +
  // uPlot.min.css) against SampleStore history queries for the history page.
  window.swBootHistoryCharts=function(){};
})();
