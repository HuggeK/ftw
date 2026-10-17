// One owner choice: statistics come with Cloud; local use asks first.
(function (window, document) {
  "use strict";
  var read = window.fetch.bind(window);
  var details = "Recorded hourly energy, configured battery capacity, battery and charger power limits, and solar capacity help improve FTW and publish group totals. Values are rounded to 10 Wh or W. No names, addresses or device ids.";
  var privacy = "Sourceful links reports to this box for deletion. They are not anonymous and stay for 13 months. Public values need at least 20 homes, with no home above 30% of a sum. Turning sharing off deletes unpublished reports when the box reaches the collector. Published totals remain.";
  var status, policy, justAnswered=false;
  function node(tag, text, parent) {
    var el = document.createElement(tag);
    if (text) el.textContent = text;
    if (parent) parent.appendChild(el);
    return el;
  }
  function say(parent, text) { var p=node("p",text,parent);p.className="hint";return p; }
  function button(parent,text,run,primary) {
    var b=node("button",text,parent);b.type="button";b.className=primary?"btn-send":"btn-ghost";
    b.addEventListener("click",function(){
      b.disabled=true;
      Promise.resolve().then(run).then(refresh).catch(function(e){say(parent,e.message||"The choice could not be saved. Check the box status.");}).finally(function(){b.disabled=false;});
    });
  }
  function write(method,path,body) {
    return window.fetch(path,{method:method,headers:{"Content-Type":"application/json"},body:body?JSON.stringify(body):undefined}).then(function(r){return r.json().then(function(v){if(!r.ok)throw Error(v.error||"The box refused the choice.");justAnswered=true;return v;});});
  }
  function paint(host,settings) {
    host.textContent="";
    if (!status || !policy || (!settings && !status.statisticsDecisionRequired && !justAnswered)) {host.hidden=true;return;}
    host.hidden=false;
    if(!settings && !status.statisticsDecisionRequired && justAnswered) {
      say(host,status.link==="ending"?"Stopping Cloud and deleting its stored copy. The box continues locally.":status.statisticsActive?"Statistics choice saved. Hourly reports wait for approval; change this in Settings → FTW app.":"Statistics sharing is off. The box continues locally.");return;
    }
    var panel=node("fieldset",null,host);
    var cloud=status.link==="on" && !status.statisticsOnly;
    node("legend",cloud?"FTW Cloud includes statistics":"Contribute to FTW statistics?",panel);
    if(cloud) say(panel,"Everyone using FTW Cloud contributes to project statistics. Accept the updated Cloud terms to use the app and remote access. To stop contributing, turn FTW Cloud off and use the box locally.");
    else say(panel,"For local FTW use, sharing is your choice. Yes is the suggested choice. Nothing starts until you answer, and saying no leaves local operation working. Statistics-only sharing gives no app, phone or assistant remote access.");
    say(panel,details);
    say(panel,privacy);
    var link=node("a","Privacy and Cloud terms",panel);link.href="https://ftw.sourceful.energy/privacy#statistics";link.target="_blank";link.rel="noopener";
    if(!policy.available) say(panel,"Hourly collection is waiting for privacy review and activation. Your choice can be saved now; no hourly report leaves before that approval.");
    var actions=node("div",null,panel);actions.className="app-link-actions";
    if(status.link==="ending") {say(panel,"Cloud is stopping and deleting its stored copy. Refresh after the box receives the deletion receipt.");return;}
    if(cloud) {
      if(status.cloudTermsAccepted) say(panel,"The current Cloud terms include statistics for this box.");
      else button(actions,"Accept Cloud terms",function(){return write("POST","/api/cloud/grant",{scopes:policy.cloud_scopes,policy:policy.cloud_policy});},true);
      button(actions,"Turn FTW Cloud off",function(){
        if(!window.confirm("Turn FTW Cloud off? The app and assistants lose remote access. The box keeps working locally. You may need to pair phones again if you turn Cloud back on."))return;
        return write("DELETE","/api/cloud");
      },false);
    } else if(status.statisticsOnly) {
      say(panel,"Local statistics sharing is on. Remote Cloud functions are off.");
      button(actions,"Stop sharing statistics",function(){return write("DELETE","/api/cloud/stats");},false);
    } else {
      button(actions,"Yes, contribute",function(){return write("POST","/api/cloud/stats",{policy:policy.policy});},true);
      if(status.statisticsDecisionRequired) button(actions,"No, keep it local",function(){return write("DELETE","/api/cloud/stats");},false);
      else say(panel,"Local statistics sharing is off.");
    }
    if(settings && !cloud) {
      var more=node("details",null,panel);node("summary","Use FTW Cloud and the app",more);
      say(more,"FTW Cloud adds app and assistant remote access and a readable state copy. Statistics are included for every Cloud user. Read the information above before accepting.");
      button(more,"Accept Cloud terms and turn it on",function(){return write("POST","/api/cloud/grant",{scopes:policy.cloud_scopes,policy:policy.cloud_policy});},true);
    }
  }
  function refresh() {
    return Promise.all([read("/api/cloud"),read("/api/cloud/stats-policy")]).then(function(rs){if(rs.some(function(r){return !r.ok;}))return null;return Promise.all(rs.map(function(r){return r.json();}));}).then(function(v){
      if(!v)return;
      status=v[0];policy=v[1];
      if(!policy.cloud_includes_statistics || !policy.standalone)return;
      var prompt=document.getElementById("project-statistics-prompt");
      if(!prompt){
        prompt=node("section");
        prompt.id="project-statistics-prompt";
        prompt.setAttribute("aria-label","Project statistics choice");
        // The Overview grid has twelve columns; the prompt needs the whole row.
        prompt.style.gridColumn="1 / -1";
        var main=document.querySelector("main")||document.body;
        main.insertBefore(prompt,main.firstChild);
      }
      paint(prompt,false);
      document.querySelectorAll("[data-project-statistics-settings]").forEach(function(host){paint(host,true);});
    }).catch(function(){});
  }
  window.FTWStatistics={refresh:refresh,cloudAccepted:function(){return !!(status && status.cloudTermsAccepted);},mount:function(host){if(status&&policy)paint(host,true);refresh();}};
  window.addEventListener("ftw-owner-ready",refresh);
  if(document.readyState==="loading")document.addEventListener("DOMContentLoaded",refresh);else refresh();
})(window,document);
