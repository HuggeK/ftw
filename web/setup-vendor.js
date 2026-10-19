(function () {
  'use strict';
  var form = document.getElementById('finish');
  var input = document.getElementById('redirect');
  var note = document.getElementById('result');
  form.addEventListener('submit', async function (event) {
    event.preventDefault();
    var button = form.querySelector('button');
    button.disabled = true;
    note.textContent = 'Finishing vendor sign-in…';
    try {
      var response = await fetch('/api/oauth/myuplink/exchange', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({redirect_url: input.value})});
      var result = await response.json();
      if (!response.ok) throw new Error(result.error || 'Could not finish sign-in.');
      input.value = '';
      note.textContent = result.status === 'saved_to_draft'
        ? 'Sign-in saved to the setup draft. These settings are not running yet. Return to your assistant to review and continue.'
        : 'Sign-in saved. Return to FTW and check the equipment readings.';
    } catch (error) {
      note.textContent = error.message + ' If the outcome is unclear, ask your assistant to read setup_status before starting again.';
    } finally { button.disabled = false; }
  });
})();
