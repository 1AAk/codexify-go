package ui

import "github.com/modelcontextprotocol/go-sdk/mcp"

const (
	SetupURI = "ui://codexify-go/setup/v1/mcp-app.html"
	DiffURI  = "ui://codexify-go/diff/v1/mcp-app.html"
	MIMEType = "text/html;profile=mcp-app"
)

func SetupToolMeta() mcp.Meta {
	return mcp.Meta{
		"ui": map[string]any{
			"resourceUri": SetupURI,
			"visibility":  []string{"model", "app"},
		},
		"ui/resourceUri":          SetupURI,
		"openai/outputTemplate":   SetupURI,
		"openai/widgetAccessible": true,
	}
}

func AppOnlyToolMeta() mcp.Meta {
	return mcp.Meta{
		"ui": map[string]any{
			"visibility": []string{"app"},
		},
		"openai/visibility":       "private",
		"openai/widgetAccessible": true,
	}
}

func DiffToolMeta() mcp.Meta {
	return mcp.Meta{
		"ui": map[string]any{
			"resourceUri": DiffURI,
			"visibility":  []string{"model"},
		},
		"ui/resourceUri": DiffURI,
	}
}

func ResourceMeta() mcp.Meta {
	return mcp.Meta{
		"ui": map[string]any{
			"prefersBorder": false,
			"csp": map[string]any{
				"connectDomains":  []string{},
				"resourceDomains": []string{},
			},
		},
		"openai/widgetPrefersBorder": false,
		"openai/widgetCSP": map[string]any{
			"connect_domains":  []string{},
			"resource_domains": []string{},
		},
	}
}

const SetupHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root{font-family:system-ui,sans-serif;color-scheme:light dark}body{margin:0;padding:12px}
.card{border:1px solid color-mix(in srgb,currentColor 18%,transparent);border-radius:12px;padding:12px}
.row{display:flex;gap:8px;flex-wrap:wrap;align-items:center}button{font:inherit;padding:7px 10px;border-radius:8px;border:1px solid color-mix(in srgb,currentColor 25%,transparent);background:transparent;cursor:pointer}
.project{display:flex;justify-content:space-between;gap:8px;padding:7px 0;border-top:1px solid color-mix(in srgb,currentColor 12%,transparent)}
.muted{opacity:.7;font-size:.9em}code{font-family:ui-monospace,monospace;font-size:.9em;overflow-wrap:anywhere}#error{color:#c44;white-space:pre-wrap}
</style>
</head>
<body>
<div class="card">
  <div class="row"><strong>Codexify Go</strong><button id="refresh">Refresh</button><button id="scratch">Scratch</button><button id="switch" hidden>Switch project</button></div>
  <div id="status" class="muted">Loading workspace status...</div>
  <div id="projects"></div>
  <div id="error"></div>
</div>
<script>
const statusEl=document.getElementById("status"),projectsEl=document.getElementById("projects"),errorEl=document.getElementById("error"),switchBtn=document.getElementById("switch");
function structured(r){return r&&((r.structuredContent)||(r.structured_content)||(r.result&&r.result.structuredContent))||null}
async function call(name,args={}){if(!(window.openai&&window.openai.callTool))throw new Error("Tool calls are unavailable in this host");return window.openai.callTool(name,args)}
async function refresh(){
  errorEl.textContent="";projectsEl.textContent="";
  try{
    const sr=await call("setup_status",{}),s=structured(sr)||{};
    const w=s.workspace||null;
    statusEl.innerHTML=w?("Selected: <code>"+escapeHTML(w.projectRoot||w.workspaceRoot||"")+"</code>"+(w.managedWorktree?" (worktree)":"")):(s.awaitingSelection?"Choose a workspace.":"No workspace selected.");
    switchBtn.hidden=!w;
    switchBtn.dataset.path=w&&w.projectRoot||"";
    const lr=await call("list_projects",{limit:40}),list=structured(lr)||{};
    for(const p of list.projects||[]){
      const div=document.createElement("div");div.className="project";
      const label=document.createElement("span");label.innerHTML="<b>"+escapeHTML(p.name||p.selector)+"</b><br><span class=muted>"+escapeHTML(p.selector)+"</span>";
      const b=document.createElement("button");b.textContent="Select";b.onclick=()=>selectProject(p.selector);
      div.append(label,b);projectsEl.append(div);
    }
  }catch(e){errorEl.textContent=String(e)}
}
async function selectProject(path){try{await call("set_project_root",{path});await refresh()}catch(e){errorEl.textContent=String(e)}}
document.getElementById("refresh").onclick=refresh;
document.getElementById("scratch").onclick=async()=>{try{await call("set_project_root",{withoutProject:true});await refresh()}catch(e){errorEl.textContent=String(e)}};
switchBtn.onclick=async()=>{try{await call("setup_ui_switch_project",{expectedPath:switchBtn.dataset.path||""});await refresh()}catch(e){errorEl.textContent=String(e)}};
function escapeHTML(v){return String(v||"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]))}
window.addEventListener("openai:set_globals",()=>refresh());
refresh();
</script>
</body>
</html>`

const DiffHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root{font-family:system-ui,sans-serif;color-scheme:light dark}body{margin:0;padding:10px}
pre{margin:0;padding:12px;border-radius:10px;background:color-mix(in srgb,currentColor 6%,transparent);overflow:auto;white-space:pre;font:12px/1.5 ui-monospace,monospace}
</style>
</head>
<body><pre id="diff">No diff output.</pre>
<script>
function render(){
 const o=(window.openai&&window.openai.toolOutput)||{};
 const v=o.output||(o.structuredContent&&o.structuredContent.output)||"No diff output.";
 document.getElementById("diff").textContent=String(v);
}
window.addEventListener("openai:set_globals",render);render();
</script></body>
</html>`
