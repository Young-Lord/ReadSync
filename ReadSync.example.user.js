// ==UserScript==
// @name         ReadSync
// @namespace    https://github.com/Young-Lord/ReadSync
// @version      1.1
// @description  将浏览的页面同步到 ReadSync 服务器
// @author       LY
// @match        *://*/*
// @grant        GM_xmlhttpRequest
// @connect      example.com
// @run-at       document-idle
// @noframes
// ==/UserScript==

(function() {
    'use strict';

    const SERVER = 'http://example.com';
    const AUTH = btoa('username:password');

    let lastUrl;

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

    // 直接劫持 document.title 的 setter：当属性被修改时立即上报。
    // 这比 MutationObserver 更精准 —— 只会在 title 实际变化时触发，
    // 不会被 head 内其他无关的 DOM 变化（如插入样式、meta 标签等）唤醒。
    var titleDescriptor = Object.getOwnPropertyDescriptor(Document.prototype, 'title');
    if (titleDescriptor && titleDescriptor.set) {
        var origTitleGet = titleDescriptor.get;
        var origTitleSet = titleDescriptor.set;
        Object.defineProperty(Document.prototype, 'title', {
            get: function() { return origTitleGet.call(this); },
            set: function(val) {
                var prev = origTitleGet.call(this);
                origTitleSet.call(this, val);
                if (val !== prev) {
                    pushTitle(window.location.href, val);
                }
            }
        });
    }
})();
