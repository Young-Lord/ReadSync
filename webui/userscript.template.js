// ==UserScript==
// @name         ReadSync
// @namespace    https://github.com/Young-Lord/ReadSync
// @version      1.2
// @description  将浏览的页面同步到 ReadSync 服务器
// @author       LY
// @match        *://*/*
// @grant        GM_xmlhttpRequest
// @connect      <!--CONNECT_HOST-->
// @run-at       document-idle
// ==/UserScript==

(function() {
    'use strict';

    const SERVER = '<!--SERVER_URL-->';
    const AUTH = '<!--AUTH_TOKEN-->';

    let lastUrl;
    let lastTitle = document.title;
    let titleTimer;

    function pushEntry(url, title) {
        GM_xmlhttpRequest({
            method: 'POST',
            url: SERVER + '/api/v1/entry',
            headers: {
                'Content-Type': 'application/json',
                'Authorization': 'Basic ' + AUTH
            },
            data: JSON.stringify({ url: url, title: title || url }),
            onload: function(res) {
                if (res.status >= 400) {
                    console.warn('[ReadSync] push failed:', res.status, res.responseText);
                }
            },
            onerror: function(err) {
                console.warn('[ReadSync] push error:', err);
            }
        });
    }

    function submitPage() {
        if (window.location.href === lastUrl) return;
        lastUrl = window.location.href;
        setTimeout(function() {
            pushEntry(window.location.href, document.title);
        }, 1000);
    }

    lastUrl = window.location.href;
    pushEntry(lastUrl, document.title);

    window.addEventListener('popstate', submitPage);

    var origPushState = history.pushState;
    history.pushState = function() {
        origPushState.apply(this, arguments);
        submitPage();
    };
    var origReplaceState = history.replaceState;
    history.replaceState = function() {
        origReplaceState.apply(this, arguments);
        submitPage();
    };

    function pushTitle(url, title) {
        GM_xmlhttpRequest({
            method: 'PATCH',
            url: SERVER + '/api/v1/entry',
            headers: {
                'Content-Type': 'application/json',
                'Authorization': 'Basic ' + AUTH
            },
            data: JSON.stringify({ url: url, title: title || url }),
            onload: function(res) {
                if (res.status >= 400) {
                    console.warn('[ReadSync] title update failed:', res.status, res.responseText);
                }
            },
            onerror: function(err) {
                console.warn('[ReadSync] title update error:', err);
            }
        });
    }

    // 监听 title 变化：debounce 后上报，后端会就地更新当前条目或新建条目。
    function onTitleChanged() {
        if (document.title === lastTitle) return;
        lastTitle = document.title;
        clearTimeout(titleTimer);
        titleTimer = setTimeout(function() {
            pushTitle(window.location.href, document.title);
        }, 600);
    }

    if (document.head) {
        new MutationObserver(onTitleChanged).observe(document.head, {
            childList: true,
            subtree: true,
            characterData: true
        });
    }
})();
