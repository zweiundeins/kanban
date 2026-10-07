/*! sb-inline-edit, version inline-edit@09d89c2e1737: its modules in one file */
import{rocket as w}from"datastar";/*!
 * From PD rockets by derekr (https://github.com/derekr/pd-rockets, v2026-09-28-2), vendored by
 * `go run ./cmd/vendorpd` with patches/pd-rockets applied, pd- names renamed to sb-.
 *
 * THE BEER-WARE LICENSE (Revision 42)
 *
 * PD rockets contributors wrote this software. As long as you retain this notice,
 * you can do whatever you want with it. If we meet someday and you think this
 * software is worth it, you can buy us a beer in return.
 */var i={tag:"sb-inline-edit",selectors:{trigger:"[data-inline-edit-trigger]",value:"[data-inline-edit-value]",input:"[data-inline-edit-input]"},events:{request:"sb-inline-edit-request",commit:"sb-inline-edit-commit",cancel:"sb-inline-edit-cancel"}};/*!
 * From PD rockets by derekr (https://github.com/derekr/pd-rockets, v2026-09-28-2), vendored by
 * `go run ./cmd/vendorpd` with patches/pd-rockets applied, pd- names renamed to sb-.
 *
 * THE BEER-WARE LICENSE (Revision 42)
 *
 * PD rockets contributors wrote this software. As long as you retain this notice,
 * you can do whatever you want with it. If we meet someday and you think this
 * software is worth it, you can buy us a beer in return.
 */w("sb-inline-edit",{mode:"light",setup({host:n,cleanup:E}){let a=0,u=!1,o=new WeakMap,d=()=>n.querySelector(i.selectors.value)?.textContent,s=()=>n.dataset.contextId??"",c=(e,t)=>n.dispatchEvent(new CustomEvent(e,{bubbles:!0,composed:!0,detail:t})),l=e=>{let t=e instanceof Element&&e.closest(i.selectors.input);return t instanceof HTMLInputElement&&n.contains(t)?t:null},f=e=>{if(e.button!==0||!(e.target instanceof Element))return;let t=e.target.closest(i.selectors.trigger);if(!t||!n.contains(t))return;let r=e.timeStamp;if(!a||r-a>500){a=r;return}a=0,c(i.events.request,{contextId:s()})},m=e=>{if(e.defaultPrevented||e.isComposing)return;let t=l(e.target);if(!t){if(e.target instanceof Element&&e.target.matches(i.selectors.trigger)&&n.contains(e.target)&&(e.key==="Enter"||e.key==="F2")){if(e.preventDefault(),e.repeat)return;c(i.events.request,{contextId:s()})}return}e.key==="Enter"?(e.preventDefault(),e.repeat||t.blur()):e.key==="Escape"&&(e.preventDefault(),u=!0,o.delete(t),c(i.events.cancel,{contextId:s()}),t.blur(),u=!1)},g=e=>{let t=l(e.target);t&&!o.has(t)&&o.set(t,d())},p=e=>{if(u)return;let t=l(e.target);t&&queueMicrotask(()=>{let r=t.getRootNode(),v=(r instanceof Document||r instanceof ShadowRoot)&&r.activeElement===t;if(!t.isConnected||v)return;let b=o.has(t)?o.get(t):d();o.delete(t),t.value===b?c(i.events.cancel,{contextId:s()}):c(i.events.commit,{contextId:s(),value:t.value})})};n.addEventListener("pointerdown",f),n.addEventListener("keydown",m),n.addEventListener("focusin",g),n.addEventListener("focusout",p),E(()=>{n.removeEventListener("pointerdown",f),n.removeEventListener("keydown",m),n.removeEventListener("focusin",g),n.removeEventListener("focusout",p)})}});
