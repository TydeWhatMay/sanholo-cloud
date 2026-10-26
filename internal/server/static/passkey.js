// Passkey sign-in and registration. No inline script is allowed by the
// content security policy, so the page carries its settings in data
// attributes and this file does the work.
(function () {
  'use strict';

  function toBuffer(s) {
    var p = s.replace(/-/g, '+').replace(/_/g, '/');
    p += '='.repeat((4 - (p.length % 4)) % 4);
    var bin = atob(p);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }

  function toText(buf) {
    var bytes = new Uint8Array(buf);
    var bin = '';
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function say(msg) {
    var el = document.querySelector('[data-passkey-status]');
    if (el) el.textContent = msg;
  }

  function post(url, csrf, body) {
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: body === undefined ? '' : JSON.stringify(body)
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || 'The request failed.');
        return j;
      });
    });
  }

  function register(csrf) {
    return post('/account/passkeys/register/begin', csrf).then(function (opts) {
      var pk = opts.publicKey;
      pk.challenge = toBuffer(pk.challenge);
      pk.user.id = toBuffer(pk.user.id);
      (pk.excludeCredentials || []).forEach(function (c) { c.id = toBuffer(c.id); });
      return navigator.credentials.create({ publicKey: pk });
    }).then(function (cred) {
      if (!cred) throw new Error('No passkey was created.');
      return post('/account/passkeys/register/finish', csrf, {
        id: cred.id,
        rawId: toText(cred.rawId),
        type: cred.type,
        response: {
          attestationObject: toText(cred.response.attestationObject),
          clientDataJSON: toText(cred.response.clientDataJSON),
          transports: cred.response.getTransports ? cred.response.getTransports() : []
        },
        clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
      });
    });
  }

  function login(csrf) {
    return post('/signin/passkey/begin', csrf).then(function (opts) {
      var pk = opts.publicKey;
      pk.challenge = toBuffer(pk.challenge);
      (pk.allowCredentials || []).forEach(function (c) { c.id = toBuffer(c.id); });
      return navigator.credentials.get({ publicKey: pk });
    }).then(function (cred) {
      if (!cred) throw new Error('No passkey was chosen.');
      var r = cred.response;
      return post('/signin/passkey/finish', csrf, {
        id: cred.id,
        rawId: toText(cred.rawId),
        type: cred.type,
        response: {
          authenticatorData: toText(r.authenticatorData),
          clientDataJSON: toText(r.clientDataJSON),
          signature: toText(r.signature),
          userHandle: r.userHandle ? toText(r.userHandle) : ''
        },
        clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
      });
    });
  }

  function supported() {
    return !!(window.PublicKeyCredential && navigator.credentials);
  }

  function reveal() {
    document.querySelectorAll('[data-passkey]').forEach(function (el) {
      el.hidden = !supported();
    });
    if (!supported()) say('This browser does not support passkeys.');
  }

  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest ? ev.target.closest('[data-passkey]') : null;
    if (!btn) return;
    ev.preventDefault();
    var csrf = btn.getAttribute('data-csrf') || '';
    var run = btn.getAttribute('data-passkey') === 'register' ? register : login;
    btn.disabled = true;
    say('');
    run(csrf).then(function (res) {
      window.location.assign(res.redirect || '/account');
    }).catch(function (err) {
      btn.disabled = false;
      say(err && err.name === 'NotAllowedError' ? 'The passkey request was cancelled.' : (err && err.message) || 'The request failed.');
    });
  });

  document.addEventListener('DOMContentLoaded', reveal);
  document.addEventListener('htmx:afterSettle', reveal);
})();
