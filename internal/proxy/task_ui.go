package proxy

import "net/http"

const taskUIHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="color-scheme" content="dark"><title>Fern Tasks</title>
<style>
:root{font-family:"Avenir Next",Avenir,"Century Gothic",sans-serif;color:#eef7df;background:#0d140c;--leaf:#b9ef86;--line:#40513a;--muted:#9baa94;--panel:#172015}
*{box-sizing:border-box}body{margin:0;min-height:100dvh;padding:max(22px,env(safe-area-inset-top)) 16px max(32px,env(safe-area-inset-bottom));background:radial-gradient(ellipse at 85% -10%,#45643a 0,transparent 42%),linear-gradient(150deg,#10190e,#090d08 70%)}
main{width:min(100%,720px);margin:auto}.mast{display:grid;grid-template-columns:1fr auto;gap:16px;align-items:end;margin:8px 2px 24px}.kicker{color:var(--leaf);font-size:12px;font-weight:800;letter-spacing:.18em;text-transform:uppercase}h1{margin:5px 0 0;font-family:Georgia,serif;font-size:clamp(38px,11vw,64px);font-weight:500;line-height:.9;letter-spacing:-.055em}.open{color:#d5dfcf;font-size:13px;text-decoration:none;border-bottom:1px solid var(--line);padding-bottom:3px}
.composer,.task{border:1px solid var(--line);background:#172015e8;box-shadow:0 20px 65px #0005}.composer{padding:20px;border-radius:26px 26px 10px 26px}.row{display:grid;grid-template-columns:1fr 130px;gap:10px}label{display:block;margin:0 0 7px;color:#b9c7b2;font-size:12px;font-weight:750;letter-spacing:.06em;text-transform:uppercase}input,textarea{width:100%;border:1px solid #53634d;border-radius:13px;background:#0d140c;color:#f4faec;font:inherit;font-size:16px;padding:13px 14px;outline:none}input:focus,textarea:focus{border-color:var(--leaf);box-shadow:0 0 0 3px #b9ef8618}textarea{min-height:138px;resize:vertical;margin-bottom:12px}button{border:0;border-radius:14px;background:var(--leaf);color:#13200e;font:inherit;font-weight:850;padding:14px 18px;cursor:pointer}.submit{width:100%;margin-top:12px}.submit[disabled]{opacity:.5;cursor:wait}
.section-title{display:flex;justify-content:space-between;align-items:center;margin:30px 3px 12px}.section-title h2{margin:0;font:600 21px Georgia,serif}.section-title button{padding:7px 10px;background:#273523;color:#cfe0c4;font-size:12px}.tasks{display:grid;gap:10px}.task{position:relative;padding:17px 18px;border-radius:10px 22px 22px 22px;overflow:hidden}.task:before{content:"";position:absolute;inset:0 auto 0 0;width:4px;background:var(--tone,#84927d)}.task-head{display:flex;gap:12px;justify-content:space-between}.task h3{margin:0;font-size:17px;letter-spacing:-.015em}.state{flex:none;color:var(--tone,#b5c0af);font-size:11px;font-weight:850;letter-spacing:.09em;text-transform:uppercase}.meta{margin-top:9px;color:var(--muted);font:12px ui-monospace,SFMono-Regular,Menlo,monospace}.task-actions{display:flex;gap:9px;margin-top:13px}.task-actions a,.task-actions button{width:auto;margin:0;padding:7px 10px;border-radius:9px;background:#293725;color:#dcebd3;font-size:12px;text-decoration:none}.task-actions .cancel{background:#492d27;color:#ffd2c8}.empty,.notice{padding:22px;border:1px dashed var(--line);border-radius:18px;color:var(--muted);line-height:1.5}.notice{display:none;margin:12px 0 0;padding:11px 13px}.notice.show{display:block}.danger{color:#ffb7a8}@media(max-width:520px){.row{grid-template-columns:1fr}.mast{align-items:start}.open{margin-top:8px}.task-head{display:block}.state{display:block;margin-top:6px}}
</style></head><body><main>
<header class="mast"><div><div class="kicker">Private work queue</div><h1>Send the next move.</h1></div><a class="open" href="/">OpenCode</a></header>
<section class="composer"><form id="composer"><label for="title">Task title</label><input id="title" maxlength="200" required placeholder="Tighten the release boundary"><label for="prompt" style="margin-top:13px">Instructions</label><textarea id="prompt" maxlength="65536" required placeholder="Describe the outcome, constraints, and checks."></textarea><div class="row"><div><label for="baseRef">Base branch</label><input id="baseRef" maxlength="255" required value="main"></div><button class="submit" id="submit" type="submit">Queue task</button></div><div class="notice" id="notice" role="status"></div></form></section>
<div class="section-title"><h2>On this phone</h2><button id="refresh" type="button">Refresh</button></div><section class="tasks" id="tasks"><div class="empty">Tasks submitted from this browser will appear here.</div></section>
</main><script src="/fern/assets/tasks.js" defer></script></body></html>`

const taskUIJS = `(()=>{"use strict";const key="fern.task.ids.v1",root=document.getElementById("tasks"),notice=document.getElementById("notice"),form=document.getElementById("composer"),submit=document.getElementById("submit");const ids=()=>{try{const value=JSON.parse(localStorage.getItem(key)||"[]");return Array.isArray(value)?value.filter(v=>typeof v==="string"&&/^tsk_[0-9a-f-]{36}$/.test(v)).slice(0,50):[]}catch{return[]}};const save=value=>localStorage.setItem(key,JSON.stringify([...new Set(value)].slice(0,50)));const show=(message,bad=false)=>{notice.textContent=message;notice.className="notice show"+(bad?" danger":"")};const tone=state=>({completed:"#b9ef86",succeeded:"#b9ef86",running:"#7fc8ff",admitted:"#7fc8ff",delivering:"#7fc8ff",input_required:"#ffd278",cancel_requested:"#ffa986",uncertain:"#ffd278",recovery_required:"#ff8d7a",failed:"#ff8d7a",canceled:"#9baa94"}[state]||"#9baa94");async function cancel(id){const response=await fetch('/fern/api/v1/tasks/'+encodeURIComponent(id)+'/cancel',{method:'POST',headers:{'Content-Type':'application/json','Idempotency-Key':crypto.randomUUID()},body:JSON.stringify({reason:'Canceled from Fern task page'})});if(!response.ok)throw new Error((await response.json().catch(()=>null))?.error?.message||'Cancellation failed');await load()}async function load(){const stored=ids();if(!stored.length){root.innerHTML='<div class="empty">Tasks submitted from this browser will appear here.</div>';return}root.replaceChildren();for(const id of stored){try{const response=await fetch('/fern/api/v1/tasks/'+encodeURIComponent(id),{headers:{Accept:'application/json'}});if(response.status===404)continue;if(!response.ok)throw new Error('Task read failed');const data=await response.json(),task=data.task,attempt=data.attempt,card=document.createElement('article');card.className='task';card.style.setProperty('--tone',tone(task.state));const head=document.createElement('div');head.className='task-head';const title=document.createElement('h3');title.textContent=task.title;const state=document.createElement('span');state.className='state';state.textContent=task.state.replaceAll('_',' ');head.append(title,state);const meta=document.createElement('div');meta.className='meta';meta.textContent=task.baseRef+' @ '+String(task.baseSha).slice(0,9)+' · attempt '+attempt.state;const actions=document.createElement('div');actions.className='task-actions';const open=document.createElement('a');open.href=attempt.openCodePath;open.textContent='Open session';actions.append(open);if(!['completed','failed','canceled'].includes(task.state)){const stop=document.createElement('button');stop.type='button';stop.className='cancel';stop.textContent='Cancel';stop.onclick=()=>cancel(task.id).catch(error=>show(error.message,true));actions.append(stop)}card.append(head,meta,actions);root.append(card)}catch{const card=document.createElement('div');card.className='empty';card.textContent='One saved task could not be refreshed.';root.append(card)}}if(!root.children.length)root.innerHTML='<div class="empty">No retained tasks were found.</div>'}form.addEventListener('submit',async event=>{event.preventDefault();submit.disabled=true;show('Resolving the exact base commit and recording your task...');try{const response=await fetch('/fern/api/v1/tasks',{method:'POST',headers:{'Content-Type':'application/json','Idempotency-Key':crypto.randomUUID()},body:JSON.stringify({title:document.getElementById('title').value,prompt:document.getElementById('prompt').value,baseRef:document.getElementById('baseRef').value})});const data=await response.json().catch(()=>null);if(!response.ok)throw new Error(data?.error?.message||'Task submission failed');save([data.task.id,...ids()]);form.reset();document.getElementById('baseRef').value='main';show('Task recorded. Fern will wake the workspace independently.');await load()}catch(error){show(error.message,true)}finally{submit.disabled=false}});document.getElementById('refresh').onclick=()=>load();load();setInterval(load,5000)})();`

func serveTaskUI(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.EscapedPath() != request.URL.Path {
		return false
	}
	switch request.URL.Path {
	case "/fern/tasks":
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			methodNotAllowed(writer, "GET, HEAD")
			return true
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(taskUIHTML))
		}
		return true
	case "/fern/assets/tasks.js":
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			methodNotAllowed(writer, "GET, HEAD")
			return true
		}
		writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		if request.Method == http.MethodGet {
			_, _ = writer.Write([]byte(taskUIJS))
		}
		return true
	default:
		return false
	}
}
