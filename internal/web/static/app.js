document.querySelectorAll('.copy-anchor').forEach(function(button){button.addEventListener('click',function(){var url=new URL(location.href);url.hash=button.dataset.anchor;navigator.clipboard&&navigator.clipboard.writeText(url.href)})})
document.querySelectorAll('.range-picker').forEach(function(form){var range=form.querySelector('[name="range"]'),dates=form.querySelectorAll('[name="from"],[name="to"]');function toggle(){dates.forEach(function(input){input.disabled=range.value!=="custom"})}range.addEventListener('change',toggle);toggle()})
var selectedAgent=document.querySelector('.agent-rail [aria-current="page"]');
if(selectedAgent&&location.hash==='#selected-agent'){selectedAgent.focus()}
