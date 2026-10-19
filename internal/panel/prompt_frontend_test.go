package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPromptViewerRejectsOutdatedResponses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `const fs=require('fs'),vm=require('vm'),assert=require('assert');
const src=fs.readFileSync(process.argv[2],'utf8');
const start=src.indexOf('function openKeyPrompts('),end=src.indexOf('/* ── 添加账号',start);
const sinks={},pending=[];
const $=id=>sinks[id]||(sinks[id]={textContent:'',innerHTML:'',value:'fresh',disabled:false,hidden:false});
const ctx={$,api:path=>new Promise((resolve,reject)=>pending.push({path,resolve,reject})),esc:String,fmtWhen:String,toast:()=>{},confirm:()=>true,loadKeys:async()=>{}};
vm.createContext(ctx);
vm.runInContext('let keyPromptId="",keyPromptData=[],keyPromptLoadSeq=0;\n'+src.slice(start,end<0?src.length:end),ctx);
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const response=(text)=>({entries:[{fresh_state:'new',messages:[{role:'user',content:text}]}]});
(async()=>{
 ctx.openKeyPrompts({id:'A'});ctx.openKeyPrompts({id:'B'});
 pending[1].resolve(response('B content'));await tick();
 pending[0].resolve(response('A content'));await tick();
 assert(sinks.keyPromptList.innerHTML.includes('B content'));assert(!sinks.keyPromptList.innerHTML.includes('A content'));
 ctx.loadKeyPrompts();sinks.keyPromptScope.value='all';ctx.loadKeyPrompts();
 pending[3].resolve(response('all content'));await tick();pending[2].resolve(response('stale scope'));await tick();
 assert(sinks.keyPromptList.innerHTML.includes('all content'));assert(!sinks.keyPromptList.innerHTML.includes('stale scope'));
 ctx.loadKeyPrompts();sinks.btnKeyPromptClose.onclick();pending[4].resolve(response('closed content'));await tick();
 assert(sinks.keyPromptBox.hidden);assert(!sinks.keyPromptList.innerHTML.includes('closed content'));
 ctx.openKeyPrompts({id:'C'});pending[5].reject(new Error('failure'));await tick();
 assert(sinks.keyPromptList.textContent.includes('读取失败'));assert(sinks.btnKeyPromptClear.disabled);
})().catch(e=>{console.error(e);process.exitCode=1});`
	path := filepath.Join(t.TempDir(), "prompt-races.cjs")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, "app.js").CombinedOutput(); err != nil {
		t.Fatalf("prompt viewer race: %v\n%s", err, out)
	}
}
