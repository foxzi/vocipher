let ws = null;
let currentChannelID = null;
let isMuted = localStorage.getItem('vocala-muted') === 'true';
let reconnectAttempts = 0;

// XSS-safe HTML escaping
const SPEAKING_LABELS = ['bzzz', 'oooo', 'aaaa', 'yoho', 'wooo', 'hehe', 'mhm', 'pew', 'rawr', 'meow', 'woof', 'yay', 'huh', 'ohno', 'blah', 'nani', 'eeek', 'gulp', 'zzz', 'bam', 'pow', 'boop', 'nom', 'uwu', 'aha', 'hmm', 'eep', 'oof', 'yip', 'arr'];
function randomSpeakingLabel() {
    return SPEAKING_LABELS[Math.floor(Math.random() * SPEAKING_LABELS.length)];
}

function escapeHTML(str) {
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
}

// --- Sound notifications (Web Audio API, no external files) ---

let notifSoundsEnabled = localStorage.getItem('vocala-sounds') !== 'off';

function playTone(freq, duration, type, vol) {
    if (!notifSoundsEnabled) return;
    try {
        const ctx = new AudioContext();
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        osc.type = type || 'sine';
        osc.frequency.value = freq;
        gain.gain.setValueAtTime(vol || 0.15, ctx.currentTime);
        gain.gain.exponentialRampToValueAtTime(0.001, ctx.currentTime + duration);
        osc.connect(gain);
        gain.connect(ctx.destination);
        osc.start();
        osc.stop(ctx.currentTime + duration);
        setTimeout(() => ctx.close(), (duration + 0.1) * 1000);
    } catch (e) {}
}

function playJoinSound() {
    playTone(520, 0.12, 'sine', 0.12);
    setTimeout(() => playTone(660, 0.12, 'sine', 0.12), 80);
    setTimeout(() => playTone(780, 0.15, 'sine', 0.10), 160);
}

function playLeaveSound() {
    playTone(660, 0.12, 'sine', 0.10);
    setTimeout(() => playTone(520, 0.15, 'sine', 0.08), 100);
}

function playChatSound() {
    playTone(880, 0.08, 'sine', 0.08);
    setTimeout(() => playTone(1100, 0.1, 'sine', 0.06), 60);
}

function toggleSounds() {
    notifSoundsEnabled = !notifSoundsEnabled;
    localStorage.setItem('vocala-sounds', notifSoundsEnabled ? 'on' : 'off');
    return notifSoundsEnabled;
}

// --- Browser notifications ---

function requestNotificationPermission() {
    if ('Notification' in window && Notification.permission === 'default') {
        Notification.requestPermission();
    }
}

function showNotification(text) {
    if (!('Notification' in window) || Notification.permission !== 'granted') return;
    if (!document.hidden) return; // Only when tab is not focused
    try {
        new Notification('Vocala', { body: text, icon: '/static/img/favicon.svg', tag: 'vocala-notif' });
    } catch (e) {}
}

// --- Local mute (per-user, client-side only) ---
const localMutedUsers = new Set(JSON.parse(localStorage.getItem('vocala-local-muted') || '[]'));

function toggleLocalMute(userId) {
    const uid = String(userId);
    if (localMutedUsers.has(uid)) {
        localMutedUsers.delete(uid);
    } else {
        localMutedUsers.add(uid);
    }
    localStorage.setItem('vocala-local-muted', JSON.stringify([...localMutedUsers]));
    // Apply to audio elements
    document.querySelectorAll('audio[data-uid]').forEach(el => {
        if (el.dataset.uid === uid) el.muted = localMutedUsers.has(uid);
    });
}

function isLocalMuted(userId) {
    return localMutedUsers.has(String(userId));
}

function forceMuteUser(userId) {
    sendWS({ type: 'force_mute', payload: { user_id: userId } });
}

// Local pixel-art avatar by username hash
const AVATAR_COUNT = 50;
function avatarURL(username) {
    let hash = 0;
    for (let i = 0; i < username.length; i++) {
        hash = ((hash << 5) - hash + username.charCodeAt(i)) | 0;
    }
    const idx = Math.abs(hash) % AVATAR_COUNT;
    return `/static/img/avatars/pa${idx}.svg`;
}

// WebRTC state
let peerConnection = null;
let localStream = null;
let micReady = false;
let pushToTalk = localStorage.getItem('vocala-ptt') === 'true';
let pttActive = false;

// VAD state
let audioContext = null;
let analyser = null;
let gainNode = null;
let processedStream = null; // audio stream routed through GainNode for VAD control
let vadInterval = null;
let isSpeaking = false;
let vadThreshold = parseInt(localStorage.getItem('vocala-vad-threshold')) || 15;
let currentVadLevel = 0;

// Screen share state
let screenStream = null;
let screenSender = null;
let screenAdaptiveCleanup = null;
let isScreenSharing = false;
let screenPreviewInterval = null;
let latestScreenPreview = null;
let screenShareUsername = null;

// Camera state
let cameraStream = null;
let cameraSender = null;
let cameraAdaptiveCleanup = null;
let isCameraOn = localStorage.getItem('vocala-camera') === 'true';

// Adaptive publisher bitrate tiers (bps). Start at tier 0, step down on
// qualityLimitationReason==='bandwidth', step up after 15s clean.
const CAMERA_BITRATE_TIERS_BPS = [1_200_000, 800_000, 500_000, 300_000, 150_000];
const SCREEN_BITRATE_TIERS_BPS = [2_500_000, 1_500_000, 800_000, 400_000];
let remoteCameras = {}; // userID -> { stream, username }
let lastServerOfferTime = 0;

// ─── WebSocket ────────────────────────────────────────────────

function connectWS() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    let wsParams = '';
    if (window.VOCALA_GUEST_CHANNEL) {
        wsParams = '?guest=1';
        if (window.VOCALA_GUEST_TOKEN) wsParams += '&token=' + encodeURIComponent(window.VOCALA_GUEST_TOKEN);
    }
    ws = new WebSocket(`${proto}//${location.host}/ws${wsParams}`);

    ws.onopen = () => {
        reconnectAttempts = 0;
        setConnectionStatus('connected');
        const dbg = document.getElementById('guest-debug');
        if (dbg) dbg.textContent = 'WS connected, joining channel ' + (window.VOCALA_GUEST_CHANNEL || 'none');

        if (currentChannelID) {
            // Rejoin channel after reconnect
            const chID = currentChannelID;
            const wasCameraOn = isCameraOn;
            cleanupWebRTC();
            currentChannelID = chID;
            sendWS({ type: 'join_channel', payload: { channel_id: chID } });
            startWebRTC().then(() => {
                if (wasCameraOn) startCamera();
            });
        } else if (window.VOCALA_GUEST_CHANNEL) {
            // Guest auto-join their assigned channel
            try {
                joinChannel(window.VOCALA_GUEST_CHANNEL, window.VOCALA_GUEST_NAME || 'Channel');
            } catch (e) {
                const dbg2 = document.getElementById('guest-debug');
                if (dbg2) dbg2.textContent = 'joinChannel error: ' + e.message;
                console.error('Guest joinChannel failed:', e);
            }
        } else if (window.VOCALA_AUTO_JOIN) {
            // Auto-join from URL on first connect
            autoJoinFromURL();
        }
    };

    ws.onclose = (e) => {
        console.warn('WS closed: code=' + e.code + ' reason=' + (e.reason || '(none)') + ' wasClean=' + e.wasClean);
        setConnectionStatus('reconnecting');
        const dbg = document.getElementById('guest-debug');
        if (dbg) dbg.textContent = 'WS closed: code=' + e.code + ' reason=' + e.reason;
        const delay = Math.min(1000 * Math.pow(2, reconnectAttempts), 30000);
        reconnectAttempts++;
        setTimeout(connectWS, delay);
    };

    ws.onerror = (e) => {
        console.error('WS error', e);
        const dbg = document.getElementById('guest-debug');
        if (dbg) dbg.textContent = 'WS error';
        ws.close();
    };

    ws.binaryType = 'arraybuffer';
    ws.onmessage = (event) => {
        if (event.data instanceof ArrayBuffer) {
            handleWSMediaFrame(event.data);
            return;
        }
        const msg = JSON.parse(event.data);
        handleWSMessage(msg);
    };
}

function handleWSMessage(msg) {
    switch (msg.type) {
        case 'error':
            if (msg.error === 'access_denied') {
                alert(msg.text || 'Access denied');
            }
            break;
        case 'channel_users':
            updateChannelUsers(msg.channel_id, msg.users || []);
            break;
        case 'presence':
            updatePresence(msg.channels || {});
            break;
        case 'webrtc_answer':
            handleWebRTCAnswer(msg.payload);
            break;
        case 'webrtc_offer':
            handleWebRTCOffer(msg.payload);
            break;
        case 'ice_candidate':
            handleRemoteICECandidate(msg.payload);
            break;
        case 'force_muted':
            // Admin force-muted you
            if (!isMuted) {
                isMuted = true;
                localStorage.setItem('vocala-muted', 'true');
                if (gainNode) gainNode.gain.value = 0.0;
                updateMuteUI();
            }
            break;
        case 'camera_on':
            // Someone turned on camera — check if we see it after delay
            setTimeout(() => {
                const el = document.getElementById('remote-cam-camera-' + msg.user_id);
                if (!el && peerConnection && peerConnection.signalingState === 'stable') {
                    console.log('Camera from user', msg.user_id, 'not received, requesting renegotiation');
                    peerConnection.createOffer().then(offer => {
                        return peerConnection.setLocalDescription(offer);
                    }).then(() => {
                        sendWS({ type: 'webrtc_offer', payload: { sdp: peerConnection.localDescription.sdp } });
                    }).catch(e => console.error('Renegotiation request failed:', e));
                }
            }, 3000);
            break;
        case 'camera_off':
            // Remove remote camera when user turns it off
            const camEl = document.getElementById('remote-cam-camera-' + msg.user_id);
            if (camEl) { camEl.remove(); updateGridColumns(); }
            break;
        case 'chat_message':
            appendChatMessage(msg);
            break;
        case 'chat_history':
            loadChatHistory(msg.messages || []);
            break;
        case 'chat_cleared':
            const chatContainer = document.getElementById('chat-messages');
            if (chatContainer) chatContainer.innerHTML = '';
            break;
        case 'chat_reaction':
            addChatReaction(msg);
            break;
        case 'screen_preview':
            // Only accept data: URIs to prevent injection via url()
            if (msg.payload.image && msg.payload.image.startsWith('data:image/')) {
                latestScreenPreview = msg.payload.image;
            }
            screenShareUsername = msg.username || null;
            // If there's already a play overlay visible, update its background
            if (document.getElementById('screen-share-play-overlay')) {
                updateScreenPreviewOverlay();
            } else if (!document.getElementById('screen-share-video') || document.getElementById('screen-share-video').classList.contains('hidden')) {
                // No video playing yet — show a preview container so user sees something is shared
                showScreenPreviewPlaceholder();
            }
            break;
        case 'screen_preview_clear':
            latestScreenPreview = null;
            screenShareUsername = null;
            removeRemoteVideo();
            break;
    }
}

function setConnectionStatus(state) {
    const el = document.getElementById('connection-status');
    const rtcEl = document.getElementById('rtc-status');
    if (el) {
        if (state === 'connected') {
            el.textContent = 'Connected';
            el.className = 'text-xs text-vc-green';
        } else if (state === 'reconnecting') {
            el.textContent = 'Reconnecting...';
            el.className = 'text-xs text-vc-yellow';
        }
    }
    if (rtcEl) updateRTCStatus();
}

function sendWS(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(msg));
    }
}

// ─── Channel Users UI ─────────────────────────────────────────

// Track users per channel for join/leave sound detection and preview
const channelUserSets = {};
const channelUsersData = {}; // channelID -> [{Username, ID, Muted, Speaking}, ...]

function updateChannelUsers(channelID, users) {
    const container = document.getElementById(`ch-users-${channelID}`);
    const countEl = document.getElementById(`ch-count-${channelID}`);

    // Store for preview
    channelUsersData[channelID] = users;

    // Update preview if currently previewing this channel
    if (previewChannelID === channelID && currentChannelID !== channelID) {
        const previewUsers = document.getElementById('preview-users');
        if (previewUsers) {
            if (users.length > 0) {
                previewUsers.innerHTML = `
                    <div class="text-sm text-vc-muted mb-2">${users.length} user${users.length > 1 ? 's' : ''} in channel:</div>
                    <div class="flex flex-wrap justify-center gap-3 mb-4">
                        ${users.map(u => `
                            <div class="flex items-center gap-2 px-3 py-1.5 bg-vc-channel rounded-lg">
                                <img src="${avatarURL(u.Username)}" class="w-6 h-6 rounded-full">
                                <span class="text-sm text-vc-text">${escapeHTML(u.Username)}</span>
                                ${u.Muted ? '<svg class="w-3 h-3 text-vc-red" fill="currentColor" viewBox="0 0 24 24"><path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/></svg>' : ''}
                                ${u.Speaking ? '<div class="flex gap-0.5"><div class="w-1 h-2 bg-vc-green rounded-full animate-pulse"></div><div class="w-1 h-3 bg-vc-green rounded-full animate-pulse" style="animation-delay:0.1s"></div></div>' : ''}
                            </div>
                        `).join('')}
                    </div>`;
            } else {
                previewUsers.innerHTML = '<div class="text-sm text-vc-muted mb-4">No one in this channel</div>';
            }
        }
    }

    // Detect join/leave in current channel for sounds + notifications
    if (channelID === currentChannelID) {
        const oldSet = channelUserSets[channelID] || new Set();
        const newSet = new Set(users.map(u => u.Username));
        const selfName = document.getElementById('self-avatar')?.dataset?.username;
        for (const name of newSet) {
            if (!oldSet.has(name) && name !== selfName && oldSet.size > 0) {
                playJoinSound();
                showNotification(name + ' joined the channel');
            }
        }
        for (const name of oldSet) {
            if (!newSet.has(name) && name !== selfName) {
                playLeaveSound();
            }
        }
        channelUserSets[channelID] = newSet;
    }

    // Sort for stable order
    users.sort((a, b) => a.Username.localeCompare(b.Username));

    // Update sidebar (may not exist for guests)
    if (container) {
        if (countEl) {
            countEl.textContent = users.length > 0 ? `${users.length} connected` : '';
        }

        const currentUsernames = new Set(users.map(u => u.Username));
        const existingItems = container.querySelectorAll('[data-sidebar-user]');
        const existingMap = {};
        existingItems.forEach(el => { existingMap[el.dataset.sidebarUser] = el; });

        existingItems.forEach(el => {
            if (!currentUsernames.has(el.dataset.sidebarUser)) el.remove();
        });

        users.forEach(u => {
            const existing = existingMap[u.Username];
            if (existing) {
                const avatar = existing.querySelector('.sb-avatar');
                if (avatar) avatar.className = `sb-avatar w-6 h-6 rounded-full ${u.Speaking ? 'ring-2 ring-vc-green/40' : ''} overflow-hidden`;
                const name = existing.querySelector('.sb-name');
                if (name) name.className = `sb-name ${u.Muted ? 'text-vc-muted line-through' : 'text-vc-text'}`;
                const muteIcon = existing.querySelector('.sb-mute');
                if (muteIcon) muteIcon.style.display = u.Muted ? '' : 'none';
                const speakingEl = existing.querySelector('.sb-speaking');
                if (speakingEl) speakingEl.style.display = u.Speaking ? '' : 'none';
            } else {
                const div = document.createElement('div');
                div.dataset.sidebarUser = u.Username;
                div.className = 'flex items-center gap-2 px-2 py-1 rounded text-sm fade-in';
                div.innerHTML = `
                    <div class="relative">
                        <div class="sb-avatar w-6 h-6 rounded-full ${u.Speaking ? 'ring-2 ring-vc-green/40' : ''} overflow-hidden">
                            <img src="${avatarURL(u.Username)}" alt="" class="w-full h-full">
                        </div>
                    </div>
                    <span class="sb-name ${u.Muted ? 'text-vc-muted line-through' : 'text-vc-text'}">${escapeHTML(u.Username)}</span>
                    <svg class="sb-mute w-3 h-3 text-vc-red ml-auto" fill="currentColor" viewBox="0 0 24 24" style="display:${u.Muted ? '' : 'none'}"><path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/></svg>
                    <div class="sb-speaking ml-auto flex gap-0.5" style="display:${u.Speaking ? '' : 'none'}"><div class="w-1 h-3 bg-vc-accent rounded-full animate-pulse"></div><div class="w-1 h-4 bg-vc-accent rounded-full animate-pulse" style="animation-delay:0.1s"></div><div class="w-1 h-2 bg-vc-accent rounded-full animate-pulse" style="animation-delay:0.2s"></div></div>
                `;
                container.appendChild(div);
            }
        });
    }

    // Update main content user cards
    if (channelID === currentChannelID) {
        updateMainContent(channelID, users);
    }
}

function updatePresence(channels) {
    for (const [chID, users] of Object.entries(channels)) {
        updateChannelUsers(parseInt(chID), users || []);
    }
}

// ─── Mobile Sidebar ───────────────────────────────────────────

function toggleSidebar() {
    const sidebar = document.getElementById('sidebar');
    const overlay = document.getElementById('sidebar-overlay');
    const isOpen = !sidebar.classList.contains('-translate-x-full');
    if (isOpen) {
        sidebar.classList.add('-translate-x-full');
        overlay.classList.add('hidden');
    } else {
        sidebar.classList.remove('-translate-x-full');
        overlay.classList.remove('hidden');
    }
}

function toggleMobileChat() {
    const panel = document.getElementById('chat-panel');
    if (!panel) return;
    panel.classList.toggle('hidden');
    panel.classList.toggle('flex');
    // Scroll to bottom when showing
    if (!panel.classList.contains('hidden')) {
        const msgs = document.getElementById('chat-messages');
        if (msgs) msgs.scrollTop = msgs.scrollHeight;
    }
}

function closeSidebarOnMobile() {
    if (window.innerWidth < 768) {
        const sidebar = document.getElementById('sidebar');
        const overlay = document.getElementById('sidebar-overlay');
        if (sidebar) sidebar.classList.add('-translate-x-full');
        if (overlay) overlay.classList.add('hidden');
    }
}

// ─── Channel Preview & Join ───────────────────────────────────

let previewChannelID = null;

function previewChannel(channelID, channelName, isPrivate) {
    // If already in this channel, do nothing
    if (currentChannelID === channelID) return;

    previewChannelID = channelID;

    // Highlight in sidebar
    document.querySelectorAll('.channel-item').forEach(el => {
        el.classList.toggle('bg-vc-hover/50', +el.dataset.channelId === channelID);
    });

    const users = channelUsersData[channelID] || [];

    const mainContent = document.getElementById('main-content');
    mainContent.innerHTML = `
        <div class="flex flex-col items-center justify-center h-full gap-6 p-6">
            <div class="w-20 h-20 rounded-2xl bg-vc-channel flex items-center justify-center">
                ${isPrivate
                    ? '<svg class="w-10 h-10 text-vc-yellow" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"/></svg>'
                    : '<svg class="w-10 h-10 text-vc-accent" fill="currentColor" viewBox="0 0 24 24"><path d="M3.9 12c0-1.71 1.39-3.1 3.1-3.1h4V7H7c-2.76 0-5 2.24-5 5s2.24 5 5 5h4v-1.9H7c-1.71 0-3.1-1.39-3.1-3.1zM8 13h8v-2H8v2zm9-6h-4v1.9h4c1.71 0 3.1 1.39 3.1 3.1s-1.39 3.1-3.1 3.1h-4V17h4c2.76 0 5-2.24 5-5s-2.24-5-5-5z"/></svg>'
                }
            </div>
            <div class="text-center">
                <h2 class="text-xl font-bold">${escapeHTML(channelName)}</h2>
                ${isPrivate ? '<span class="text-xs text-vc-yellow">Private channel</span>' : ''}
            </div>
            <div id="preview-users" class="text-center">
                ${users.length > 0
                    ? `<div class="text-sm text-vc-muted mb-2">${users.length} user${users.length > 1 ? 's' : ''} in channel:</div>
                       <div class="flex flex-wrap justify-center gap-3 mb-4">
                           ${users.map(u => `
                               <div class="flex items-center gap-2 px-3 py-1.5 bg-vc-channel rounded-lg">
                                   <img src="${avatarURL(u.Username)}" class="w-6 h-6 rounded-full">
                                   <span class="text-sm text-vc-text">${escapeHTML(u.Username)}</span>
                                   ${u.Muted ? '<svg class="w-3 h-3 text-vc-red" fill="currentColor" viewBox="0 0 24 24"><path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/></svg>' : ''}
                                   ${u.Speaking ? '<div class="flex gap-0.5"><div class="w-1 h-2 bg-vc-green rounded-full animate-pulse"></div><div class="w-1 h-3 bg-vc-green rounded-full animate-pulse" style="animation-delay:0.1s"></div></div>' : ''}
                               </div>
                           `).join('')}
                       </div>`
                    : '<div class="text-sm text-vc-muted mb-4">No one in this channel</div>'
                }
            </div>
            <button data-action="join-channel" data-ch-id="${channelID}" data-ch-name="${escapeHTML(channelName)}"
                class="px-6 py-2.5 bg-vc-accent hover:bg-vc-accent/80 text-white font-medium rounded-lg transition flex items-center gap-2">
                <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M11 16l-4-4m0 0l4-4m-4 4h14m-5 4v1a3 3 0 01-3 3H6a3 3 0 01-3-3V7a3 3 0 013-3h7a3 3 0 013 3v1"/>
                </svg>
                Join Channel
            </button>
        </div>
    `;
}

function joinChannel(channelID, channelName) {
    if (currentChannelID === channelID) return;

    document.querySelectorAll('.channel-item').forEach(el => {
        el.classList.remove('bg-vc-hover/50');
    });
    const item = document.querySelector(`[data-channel-id="${channelID}"]`);
    if (item) item.classList.add('bg-vc-hover/50');

    // Cleanup previous WebRTC
    cleanupWebRTC();

    currentChannelID = channelID;
    sendWS({ type: 'join_channel', payload: { channel_id: channelID } });

    // Update URL to permanent link
    if (!window.VOCALA_GUEST_CHANNEL) {
        history.pushState({ channelID, channelName }, '', '/channels/' + encodeURIComponent(channelName));
    }

    // Close sidebar on mobile and update mobile header
    closeSidebarOnMobile();
    const mobileChName = document.getElementById('mobile-channel-name');
    if (mobileChName) mobileChName.textContent = channelName;

    const mainContent = document.getElementById('main-content');
    mainContent.innerHTML = `
        <div class="w-full h-full flex flex-col">
            <div class="px-4 md:px-6 py-3 border-b border-vc-border flex items-center gap-2 md:gap-3">
                <svg class="w-5 h-5 md:w-6 md:h-6 text-vc-accent flex-shrink-0" fill="currentColor" viewBox="0 0 24 24">
                    <path d="M3.9 12c0-1.71 1.39-3.1 3.1-3.1h4V7H7c-2.76 0-5 2.24-5 5s2.24 5 5 5h4v-1.9H7c-1.71 0-3.1-1.39-3.1-3.1zM8 13h8v-2H8v2zm9-6h-4v1.9h4c1.71 0 3.1 1.39 3.1 3.1s-1.39 3.1-3.1 3.1h-4V17h4c2.76 0 5-2.24 5-5s-2.24-5-5-5z"/>
                </svg>
                <h2 class="text-base md:text-xl font-bold truncate">${escapeHTML(channelName)}</h2>
                <div id="rtc-status" class="flex items-center gap-1.5 ml-2 flex-shrink-0">
                    <div class="w-2 h-2 rounded-full bg-vc-yellow animate-pulse"></div>
                    <span class="text-xs text-vc-yellow">Connecting...</span>
                </div>
                ${window.VOCALA_GUEST_CHANNEL ? '' : `<button onclick="createGuestLink(${channelID})" class="ml-auto px-3 py-1.5 bg-vc-channel hover:bg-vc-hover text-vc-muted hover:text-vc-text text-xs md:text-sm rounded-lg transition flex-shrink-0 border border-vc-border" title="Generate guest invite link">
                    Guest Link
                </button>`}
                <button onclick="leaveChannel()" class="px-3 py-1.5 bg-vc-red/20 hover:bg-vc-red/30 text-vc-red text-xs md:text-sm font-medium rounded-lg transition flex-shrink-0">
                    Leave
                </button>
            </div>
            <div class="flex-1 flex flex-col md:flex-row overflow-hidden">
                <!-- Voice/Video area -->
                <div class="flex-1 flex flex-col overflow-y-auto p-3 md:p-6 min-h-0">
                    <div id="screen-share-anchor"></div>
                    <div class="flex-1 flex items-center justify-center" id="channel-view-users">
                        <div class="text-center text-vc-muted">
                            <p>Joining channel...</p>
                        </div>
                    </div>
                </div>
                <!-- Chat panel (hidden on mobile by default, toggle with button) -->
                <div id="chat-panel" class="w-full md:w-80 border-t md:border-t-0 md:border-l border-vc-border flex flex-col bg-vc-sidebar/30 max-h-64 md:max-h-none hidden md:flex">
                    <div class="px-3 py-2 border-b border-vc-border flex items-center gap-2">
                        <svg class="w-4 h-4 text-vc-muted" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z"/>
                        </svg>
                        <span class="text-xs font-medium text-vc-muted">Chat</span>
                        ${window.VOCALA_IS_ADMIN ? `<button onclick="clearChat()" class="ml-auto text-[10px] text-vc-muted hover:text-vc-red transition" title="Clear chat history">Clear</button>` : ''}
                        <button onclick="toggleMobileChat()" class="ml-auto md:hidden text-vc-muted hover:text-vc-text">
                            <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/></svg>
                        </button>
                    </div>
                    <div id="chat-messages" class="flex-1 overflow-y-auto p-2 space-y-1 min-h-0"></div>
                    <div class="p-2 border-t border-vc-border">
                        <form onsubmit="sendChatMessage(event)" class="flex gap-1.5">
                            <input type="text" id="chat-input" placeholder="Message..." autocomplete="off"
                                class="flex-1 px-2.5 py-1.5 bg-vc-bg border border-vc-border rounded-lg text-sm text-vc-text placeholder-vc-muted focus:outline-none focus:border-vc-accent transition">
                            <button type="submit" class="px-2.5 py-1.5 bg-vc-accent hover:bg-vc-accent/80 text-white rounded-lg transition">
                                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 19l9 2-9-18-9 18 9-2zm0 0v-8"/>
                                </svg>
                            </button>
                        </form>
                    </div>
                </div>
            </div>
            <div class="px-3 md:px-6 py-2 md:py-3 border-t border-vc-border bg-vc-sidebar/50">
                <!-- Row 1: Main buttons -->
                <div class="flex items-center justify-center gap-2 md:gap-4">
                    <button onclick="toggleMute()" id="main-mute-btn"
                        class="flex items-center gap-1.5 px-3 py-2 rounded-lg ${isMuted ? 'bg-vc-red/20 text-vc-red' : 'bg-vc-channel hover:bg-vc-hover text-vc-text'} transition text-sm">
                        <svg class="w-5 h-5" id="main-icon-mic" fill="currentColor" viewBox="0 0 24 24">
                            ${isMuted ?
                                '<path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.08c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/>' :
                                '<path d="M12 14c1.66 0 3-1.34 3-3V5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3z"/><path d="M17 11c0 2.76-2.24 5-5 5s-5-2.24-5-5H5c0 3.53 2.61 6.43 6 6.92V21h2v-3.08c3.39-.49 6-3.39 6-6.92h-2z"/>'}
                        </svg>
                        <span id="main-mute-text" class="hidden md:inline">${isMuted ? 'Unmute' : 'Mute'}</span>
                    </button>
                    <button onclick="toggleCamera()" id="camera-btn"
                        class="flex items-center gap-1.5 px-3 py-2 rounded-lg bg-vc-channel hover:bg-vc-hover text-vc-text transition text-sm">
                        <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 10l4.553-2.276A1 1 0 0121 8.618v6.764a1 1 0 01-1.447.894L15 14M5 18h8a2 2 0 002-2V8a2 2 0 00-2-2H5a2 2 0 00-2 2v8a2 2 0 002 2z"/>
                        </svg>
                        <span class="hidden md:inline">Camera</span>
                    </button>
                    <button onclick="isScreenSharing ? stopScreenShare() : startScreenShare()" id="screen-share-btn"
                        class="flex items-center gap-1.5 px-3 py-2 rounded-lg bg-vc-channel hover:bg-vc-hover text-vc-text transition text-sm">
                        <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z"/>
                        </svg>
                        <span class="hidden md:inline">Screen</span>
                    </button>
                    <button onclick="togglePTT()" id="ptt-btn"
                        class="flex items-center gap-1.5 px-3 py-2 rounded-lg ${pushToTalk ? 'bg-vc-accent/20 text-vc-accent' : 'bg-vc-channel hover:bg-vc-hover text-vc-muted'} transition text-sm">
                        <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 17h5l-1.405-1.405A2.032 2.032 0 0118 14.158V11a6.002 6.002 0 00-4-5.659V5a2 2 0 10-4 0v.341C7.67 6.165 6 8.388 6 11v3.159c0 .538-.214 1.055-.595 1.436L4 17h5m6 0v1a3 3 0 11-6 0v-1m6 0H9"/>
                        </svg>
                        <span class="hidden md:inline">PTT ${pushToTalk ? 'ON' : 'OFF'}</span>
                    </button>
                    <button onclick="toggleMobileChat()" class="md:hidden flex items-center gap-1.5 px-3 py-2 rounded-lg bg-vc-channel hover:bg-vc-hover text-vc-text transition text-sm">
                        <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z"/>
                        </svg>
                    </button>
                    <div class="text-xs text-vc-muted hidden md:block" id="ptt-hint">${pushToTalk ? 'Hold Space to talk' : ''}</div>
                </div>
                <!-- Row 2: Sensitivity -->
                <div class="flex items-center gap-2 mt-2 justify-center">
                    <span class="text-xs text-vc-muted flex-shrink-0">Sensitivity</span>
                    <input type="range" min="1" max="60" value="${vadThreshold}" oninput="setVadThreshold(this.value)"
                        class="w-20 md:w-36 h-1.5 rounded-full appearance-none bg-vc-border cursor-pointer accent-vc-accent">
                    <div class="relative w-16 h-2 bg-vc-bg rounded-full overflow-hidden border border-vc-border flex-shrink-0">
                        <div id="vad-meter" class="h-full rounded-full bg-vc-muted/50 transition-all duration-75" style="width:0%"></div>
                        <div id="vad-threshold-marker" class="absolute top-0 h-full w-0.5 bg-vc-accent/80" style="left:${Math.min(100, (vadThreshold / 80) * 100)}%"></div>
                    </div>
                </div>
            </div>
        </div>
    `;

    // Start WebRTC (TCP candidates available for mobile)
    startWebRTC().then(() => {
        // Restore camera if it was on before page reload
        if (localStorage.getItem('vocala-camera') === 'true' && !isCameraOn) {
            startCamera();
        }
    });
}

let lastChannelUsers = [];

function updateMainContent(channelID, users) {
    const container = document.getElementById('channel-view-users');
    if (!container) return;
    lastChannelUsers = users;

    // Clean up camera grid for users who left
    const cameraGrid = document.getElementById('camera-grid');
    if (cameraGrid) {
        const userIds = new Set(users.map(u => String(u.ID)));
        cameraGrid.querySelectorAll('[id^="remote-cam-camera-"]').forEach(el => {
            // id = "remote-cam-camera-{userID}", extract userID
            const uid = el.id.replace('remote-cam-camera-', '');
            if (!userIds.has(uid)) el.remove();
        });
        updateGridColumns();
    }

    // Sort users consistently by username to prevent reordering
    users.sort((a, b) => a.Username.localeCompare(b.Username));

    if (users.length === 0) {
        container.innerHTML = `
            <div class="text-center text-vc-muted">
                <p class="text-lg font-medium">Nobody here yet</p>
                <p class="text-sm mt-1">Invite your friends to join!</p>
            </div>
        `;
        return;
    }

    // Check if grid already exists — if so, update in place
    let grid = container.querySelector('.user-grid');
    if (!grid) {
        grid = document.createElement('div');
        grid.className = 'user-grid grid grid-cols-2 sm:grid-cols-3 md:grid-cols-4 lg:grid-cols-5 gap-6';
        container.innerHTML = '';
        container.appendChild(grid);
    }

    const existingCards = grid.querySelectorAll('[data-username]');
    const existingMap = {};
    existingCards.forEach(card => { existingMap[card.dataset.username] = card; });

    const currentUsernames = new Set(users.map(u => u.Username));

    // Remove users no longer present
    existingCards.forEach(card => {
        if (!currentUsernames.has(card.dataset.username)) {
            card.remove();
        }
    });

    // Add or update each user
    users.forEach(u => {
        const existing = existingMap[u.Username];
        if (existing) {
            // Update in place — only change classes/content that differ
            const lmuted = isLocalMuted(u.ID);
            const border = u.Speaking ? 'border-vc-green shadow-lg shadow-vc-green/20' : 'border-vc-border';
            existing.className = `flex flex-col items-center gap-3 p-4 rounded-xl bg-vc-sidebar/50 border ${border} transition-all duration-200 ${lmuted ? 'opacity-50' : ''}`;
            const lmuteBtn = existing.querySelector('.local-mute-btn');
            if (lmuteBtn) {
                lmuteBtn.textContent = lmuted ? 'Unmute' : 'Mute';
                lmuteBtn.className = `local-mute-btn text-[10px] px-2 py-0.5 rounded ${lmuted ? 'bg-vc-red/20 text-vc-red' : 'bg-vc-channel text-vc-muted hover:text-vc-red hover:bg-vc-red/10'} transition`;
            }

            const avatar = existing.querySelector('.avatar-circle');
            if (avatar) {
                avatar.className = `avatar-circle w-16 h-16 rounded-full ${u.Speaking ? 'ring-4 ring-vc-green/30' : ''} overflow-hidden transition-all`;
            }

            const muteIndicator = existing.querySelector('.mute-indicator');
            if (muteIndicator) muteIndicator.style.display = u.Muted ? '' : 'none';

            const nameEl = existing.querySelector('.user-name');
            if (nameEl) nameEl.className = `user-name text-sm font-medium ${u.Muted ? 'text-vc-muted' : 'text-vc-text'}`;

            const speakingIndicator = existing.querySelector('.speaking-indicator');
            if (speakingIndicator) speakingIndicator.style.display = u.Speaking ? '' : 'none';
            const speakingLabel = existing.querySelector('.speaking-label');
            if (speakingLabel) {
                if (u.Speaking) {
                    if (!speakingLabel.dataset.active) {
                        speakingLabel.textContent = randomSpeakingLabel();
                        speakingLabel.dataset.active = '1';
                    }
                } else {
                    delete speakingLabel.dataset.active;
                }
            }
            const spacer = existing.querySelector('.speaking-spacer');
            if (spacer) spacer.style.display = u.Speaking ? 'none' : '';
        } else {
            // New user — create card with fade-in
            const selfName = document.getElementById('self-avatar')?.dataset?.username || window.VOCALA_GUEST_NAME;
            const isSelf = u.Username === selfName;
            const lmuted = isLocalMuted(u.ID);
            const card = document.createElement('div');
            card.dataset.username = u.Username;
            card.dataset.userId = u.ID;
            card.className = `flex flex-col items-center gap-3 p-4 rounded-xl bg-vc-sidebar/50 border ${u.Speaking ? 'border-vc-green shadow-lg shadow-vc-green/20' : 'border-vc-border'} fade-in transition-all duration-200 ${lmuted ? 'opacity-50' : ''}`;
            card.innerHTML = `
                <div class="relative">
                    <div class="avatar-circle w-16 h-16 rounded-full ${u.Speaking ? 'ring-4 ring-vc-green/30' : ''} overflow-hidden transition-all">
                        <img src="${avatarURL(u.Username)}" alt="" class="w-full h-full">
                    </div>
                    <div class="mute-indicator absolute -bottom-1 -right-1 w-6 h-6 rounded-full bg-vc-red flex items-center justify-center" style="display:${u.Muted ? '' : 'none'}"><svg class="w-3 h-3 text-white" fill="currentColor" viewBox="0 0 24 24"><path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/></svg></div>
                </div>
                <span class="user-name text-sm font-medium ${u.Muted ? 'text-vc-muted' : 'text-vc-text'}">${escapeHTML(u.Username)}</span>
                ${!isSelf ? `<div class="flex gap-1">
                    <button class="local-mute-btn text-[10px] px-2 py-0.5 rounded ${lmuted ? 'bg-vc-red/20 text-vc-red' : 'bg-vc-channel text-vc-muted hover:text-vc-red hover:bg-vc-red/10'} transition" onclick="toggleLocalMute(${u.ID}); updateMainContent(currentChannelID, lastChannelUsers);">${lmuted ? 'Unmute' : 'Mute'}</button>
                    ${window.VOCALA_IS_ADMIN && !u.Muted ? `<button class="text-[10px] px-2 py-0.5 rounded bg-vc-channel text-vc-muted hover:text-vc-yellow hover:bg-vc-yellow/10 transition" onclick="forceMuteUser(${u.ID})" title="Force mute for everyone">Force</button>` : ''}
                </div>` : ''}
                <div class="speaking-indicator flex items-center gap-1.5" style="display:${u.Speaking ? '' : 'none'}">
                    <div class="flex gap-0.5"><div class="w-1.5 h-3 bg-vc-green rounded-full animate-pulse"></div><div class="w-1.5 h-5 bg-vc-green rounded-full animate-pulse" style="animation-delay:0.15s"></div><div class="w-1.5 h-3 bg-vc-green rounded-full animate-pulse" style="animation-delay:0.3s"></div></div>
                    <span class="speaking-label text-xs text-vc-green font-medium" data-active="${u.Speaking ? '1' : ''}">${u.Speaking ? randomSpeakingLabel() : ''}</span>
                </div>
                <div class="speaking-spacer h-5" style="display:${u.Speaking ? 'none' : ''}"></div>
            `;
            grid.appendChild(card);
        }
    });
}

function leaveChannel() {
    if (!currentChannelID) return;
    sendWS({ type: 'leave_channel' });
    currentChannelID = null;
    localStorage.setItem('vocala-camera', 'false');
    cleanupWebRTC();

    // Reset URL
    history.pushState({}, '', '/');

    document.querySelectorAll('.channel-item').forEach(el => {
        el.classList.remove('bg-vc-hover/50');
    });

    const mobileChName = document.getElementById('mobile-channel-name');
    if (mobileChName) mobileChName.textContent = 'Select a channel';

    document.getElementById('main-content').innerHTML = `
        <div class="text-center text-vc-muted">
            <svg class="w-20 h-20 mx-auto mb-4 opacity-20" fill="currentColor" viewBox="0 0 24 24">
                <path d="M12 14c1.66 0 3-1.34 3-3V5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3z"/>
                <path d="M17 11c0 2.76-2.24 5-5 5s-5-2.24-5-5H5c0 3.53 2.61 6.43 6 6.92V21h2v-3.08c3.39-.49 6-3.39 6-6.92h-2z"/>
            </svg>
            <p class="text-lg font-medium">Select a voice channel</p>
            <p class="text-sm mt-1">Click a channel to join and start talking</p>
        </div>
    `;
}

async function deleteChannel(channelId, channelName) {
    if (!confirm('Delete channel "' + channelName + '"? This cannot be undone.')) return;

    const form = new FormData();
    form.append('id', channelId);
    form.append('csrf_token', getCSRFToken());

    try {
        const res = await fetch('/channels/delete', { method: 'POST', body: form });
        if (!res.ok) {
            alert('Failed to delete channel');
            return;
        }
        // If we're in this channel, leave it
        if (currentChannelID === channelId) {
            leaveChannel();
        }
        // Refresh channel list with HTMX response
        document.getElementById('channel-list').innerHTML = await res.text();
    } catch (err) {
        console.error('Failed to delete channel:', err);
    }
}

async function toggleChannelPrivacy(channelId, channelName, currentlyPrivate) {
    const next = !currentlyPrivate;
    const verb = next ? 'private' : 'public';
    if (!confirm('Make channel "' + channelName + '" ' + verb + '?')) return;

    const form = new FormData();
    form.append('id', channelId);
    form.append('is_private', next ? 'true' : 'false');
    form.append('csrf_token', getCSRFToken());

    try {
        const res = await fetch('/channels/privacy', { method: 'POST', body: form });
        if (!res.ok) {
            alert('Failed to change channel privacy');
            return;
        }
        document.getElementById('channel-list').innerHTML = await res.text();
    } catch (err) {
        console.error('Failed to toggle channel privacy:', err);
    }
}

// ─── Mute / PTT ───────────────────────────────────────────────

function toggleMute() {
    // Can't unmute without mic access
    if (!localStream && isMuted) return;

    isMuted = !isMuted;
    localStorage.setItem('vocala-muted', isMuted);
    sendWS({ type: 'mute', payload: { muted: isMuted } });

    // Mute/unmute via GainNode
    if (gainNode) {
        gainNode.gain.value = isMuted ? 0.0 : 1.0;
    }

    updateMuteUI();
}

function togglePTT() {
    pushToTalk = !pushToTalk;
    localStorage.setItem('vocala-ptt', pushToTalk);
    const btn = document.getElementById('ptt-btn');
    const hint = document.getElementById('ptt-hint');
    if (btn) {
        btn.className = `flex items-center gap-2 px-4 py-2 rounded-lg ${pushToTalk ? 'bg-vc-accent/20 text-vc-accent' : 'bg-vc-channel hover:bg-vc-hover text-vc-muted'} transition text-sm`;
        btn.innerHTML = `
            <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 17h5l-1.405-1.405A2.032 2.032 0 0118 14.158V11a6.002 6.002 0 00-4-5.659V5a2 2 0 10-4 0v.341C7.67 6.165 6 8.388 6 11v3.159c0 .538-.214 1.055-.595 1.436L4 17h5m6 0v1a3 3 0 11-6 0v-1m6 0H9"/>
            </svg>
            PTT ${pushToTalk ? 'ON' : 'OFF'}`;
    }
    if (hint) hint.textContent = pushToTalk ? 'Hold Space to talk' : '';

    if (gainNode) {
        gainNode.gain.value = pushToTalk ? 0.0 : (isMuted ? 0.0 : 1.0);
    }
}

// ─── WebRTC ───────────────────────────────────────────────────

async function startWebRTC() {
    try {
        // Get microphone access
        const audioConstraints = {
            echoCancellation: true,
            noiseSuppression: true,
            autoGainControl: true,
        };
        if (selectedMicId) audioConstraints.deviceId = { exact: selectedMicId };
        localStream = await navigator.mediaDevices.getUserMedia({
            audio: audioConstraints,
            video: false,
        });

        // Route audio through Web Audio API GainNode for VAD-based muting
        audioContext = new AudioContext();
        const source = audioContext.createMediaStreamSource(localStream);
        gainNode = audioContext.createGain();
        gainNode.gain.value = (pushToTalk || isMuted) ? 0.0 : 1.0;
        const dest = audioContext.createMediaStreamDestination();
        source.connect(gainNode);
        gainNode.connect(dest);
        processedStream = dest.stream;

        // Setup VAD (reads from raw localStream for level detection)
        setupVAD(localStream);

        // Create peer connection with server-provided ICE config (includes TURN if configured)
        const iceServers = window.VOCALA_ICE_SERVERS || [
            { urls: 'stun:stun.l.google.com:19302' },
            { urls: 'stun:stun1.l.google.com:19302' },
        ];
        // Force relay on mobile if TURNS is available (carrier NAT drops UDP)
        const isMobile = /Android|iPhone|iPad|iPod/i.test(navigator.userAgent);
        const hasTurns = iceServers.some(s => {
            const urls = Array.isArray(s.urls) ? s.urls : [s.urls || ''];
            return urls.some(u => u.startsWith('turns:'));
        });
        const rtcConfig = { iceServers };
        if (isMobile && hasTurns) {
            rtcConfig.iceTransportPolicy = 'relay';
            console.log('Mobile detected, forcing TURNS relay');
        }
        peerConnection = new RTCPeerConnection(rtcConfig);

        // Add processed audio track (goes through GainNode)
        processedStream.getTracks().forEach(track => {
            peerConnection.addTrack(track, processedStream);
        });

        // Handle remote tracks
        peerConnection.ontrack = (event) => {
            if (event.track.kind === 'audio') {
                const audio = new Audio();
                audio.srcObject = event.streams[0];
                audio.autoplay = true;
                if (selectedSpkId && audio.setSinkId) {
                    audio.setSinkId(selectedSpkId).catch(() => {});
                }
                // Extract userID from stream.id "audio-{userID}"
                const audioStreamId = event.streams[0]?.id || '';
                const uid = audioStreamId.replace('audio-', '');
                audio.dataset.streamId = audioStreamId;
                audio.dataset.uid = uid;
                // Apply local mute if user was muted
                if (localMutedUsers.has(uid)) audio.muted = true;
                document.body.appendChild(audio);
                audio.play().catch(() => {});
            } else if (event.track.kind === 'video') {
                const stream = event.streams[0] || new MediaStream([event.track]);
                const streamId = stream.id || '';

                if (streamId.startsWith('camera')) {
                    // Remote camera — add to camera grid
                    // Use mid (media line ID) as stable identifier
                    const mid = event.transceiver ? event.transceiver.mid : null;
                    handleRemoteCameraTrack(stream, event.track, mid);
                } else {
                    // Screen share — showRemoteVideo handles dedup by stream.id
                    showRemoteVideo(stream, event.track);
                }
            }
        };

        // ICE candidates
        peerConnection.onicecandidate = (event) => {
            if (event.candidate) {
                sendWS({
                    type: 'ice_candidate',
                    payload: { candidate: event.candidate.toJSON() },
                });
            }
        };

        // Renegotiation needed (e.g. after addTrack/removeTrack)
        let negoTimeout = null;
        peerConnection.onnegotiationneeded = async () => {
            // Debounce to coalesce multiple track additions
            if (negoTimeout) clearTimeout(negoTimeout);
            negoTimeout = setTimeout(async () => {
                try {
                    if (!peerConnection) return;
                    // Wait for stable state (server offer may be in progress)
                    for (let i = 0; i < 20; i++) {
                        if (peerConnection.signalingState === 'stable') break;
                        await new Promise(r => setTimeout(r, 200));
                    }
                    if (!peerConnection || peerConnection.signalingState !== 'stable') return;
                    const offer = await peerConnection.createOffer();
                    if (peerConnection.signalingState !== 'stable') return;
                    await peerConnection.setLocalDescription(offer);
                    sendWS({ type: 'webrtc_offer', payload: { sdp: offer.sdp } });
                } catch (err) {
                    console.error('Negotiation failed:', err);
                }
            }, 500);
        };

        // Connection state
        peerConnection.onconnectionstatechange = () => {
            updateRTCStatus();
        };

        peerConnection.oniceconnectionstatechange = () => {
            updateRTCStatus();
        };

        // Create and send offer
        const offer = await peerConnection.createOffer();
        await peerConnection.setLocalDescription(offer);

        sendWS({
            type: 'webrtc_offer',
            payload: { sdp: offer.sdp },
        });

        // Restore saved state
        sendWS({ type: 'mute', payload: { muted: isMuted } });
        updateMuteUI();

    } catch (err) {
        console.error('WebRTC mic setup failed, trying receive-only:', err);
        // Create peer connection without microphone (receive-only mode)
        // This allows seeing camera/screen/hearing audio from others
        try {
            const iceServers = window.VOCALA_ICE_SERVERS || [
                { urls: 'stun:stun.l.google.com:19302' },
            ];
            const rtcConfig = { iceServers };
            peerConnection = new RTCPeerConnection(rtcConfig);

            // Add receive-only audio transceiver
            peerConnection.addTransceiver('audio', { direction: 'recvonly' });

            peerConnection.ontrack = (event) => {
                if (event.track.kind === 'audio') {
                    const audio = new Audio();
                    audio.srcObject = event.streams[0];
                    audio.autoplay = true;
                    if (selectedSpkId && audio.setSinkId) {
                        audio.setSinkId(selectedSpkId).catch(() => {});
                    }
                    const uid = (event.streams[0]?.id || '').replace('audio-', '');
                    audio.dataset.uid = uid;
                    if (localMutedUsers.has(uid)) audio.muted = true;
                    document.body.appendChild(audio);
                    audio.play().catch(() => {});
                } else if (event.track.kind === 'video') {
                    const stream = event.streams[0] || new MediaStream([event.track]);
                    const streamId = stream.id || '';
                    if (streamId.startsWith('camera')) {
                        const mid = event.transceiver ? event.transceiver.mid : null;
                        handleRemoteCameraTrack(stream, event.track, mid);
                    } else {
                        showRemoteVideo(stream, event.track);
                    }
                }
            };

            peerConnection.onicecandidate = (event) => {
                if (event.candidate) {
                    sendWS({ type: 'ice_candidate', payload: { candidate: event.candidate } });
                }
            };

            const offer = await peerConnection.createOffer();
            await peerConnection.setLocalDescription(offer);
            sendWS({ type: 'webrtc_offer', payload: { sdp: offer.sdp } });
            updateRTCStatusText('connected', 'Listen-only (no mic)');
        } catch (err2) {
            console.error('Receive-only WebRTC also failed:', err2);
            updateRTCStatusText('error', 'WebRTC failed');
        }

        if (!isMuted) {
            isMuted = true;
            sendWS({ type: 'mute', payload: { muted: true } });
            updateMuteUI();
        }
    }
}

function updateMuteUI() {
    // Update sidebar icons (may not exist for guests)
    const micIcon = document.getElementById('icon-mic');
    const micOffIcon = document.getElementById('icon-mic-off');
    if (micIcon) micIcon.classList.toggle('hidden', isMuted);
    if (micOffIcon) micOffIcon.classList.toggle('hidden', !isMuted);

    // Update main content button
    const mainBtn = document.getElementById('main-mute-btn');
    const mainText = document.getElementById('main-mute-text');
    const mainIcon = document.getElementById('main-icon-mic');
    if (mainBtn) {
        mainBtn.className = `flex items-center gap-2 px-4 py-2 rounded-lg ${isMuted ? 'bg-vc-red/20 text-vc-red' : 'bg-vc-channel hover:bg-vc-hover text-vc-text'} transition`;
    }
    if (mainText) mainText.textContent = isMuted ? 'Unmute' : 'Mute';
    if (mainIcon) {
        mainIcon.innerHTML = isMuted ?
            '<path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/>' :
            '<path d="M12 14c1.66 0 3-1.34 3-3V5c0-1.66-1.34-3-3-3S9 3.34 9 5v6c0 1.66 1.34 3 3 3z"/><path d="M17 11c0 2.76-2.24 5-5 5s-5-2.24-5-5H5c0 3.53 2.61 6.43 6 6.92V21h2v-3.08c3.39-.49 6-3.39 6-6.92h-2z"/>';
    }
}

// ICE candidates from the server can arrive before we've processed the
// server's SDP answer (pion's OnICECandidate fires inside SetLocalDescription,
// so candidates can be enqueued on the WS before the answer message). We
// buffer them here and flush once a remoteDescription is set.
let pendingIceCandidates = [];

function flushPendingIce() {
    if (!peerConnection) { pendingIceCandidates = []; return; }
    const pending = pendingIceCandidates;
    pendingIceCandidates = [];
    for (const c of pending) {
        peerConnection.addIceCandidate(new RTCIceCandidate(c))
            .catch(err => console.warn('Flushed ICE candidate failed:', err));
    }
}

async function handleWebRTCAnswer(payload) {
    if (!peerConnection) return;
    try {
        await peerConnection.setRemoteDescription(
            new RTCSessionDescription({ type: 'answer', sdp: payload.sdp })
        );
        flushPendingIce();
    } catch (err) {
        console.error('Failed to set remote description:', err);
    }
}

async function handleWebRTCOffer(payload) {
    // Server-initiated renegotiation (new peer joined or tracks changed)
    if (!peerConnection) return;
    lastServerOfferTime = Date.now();

    try {
        // Perfect-negotiation: client is the "polite" peer. If we have our
        // own offer in flight (have-local-offer), rollback and accept the
        // server's offer. The server is "impolite" and drops client offers
        // on glare, so rolling back here is what breaks the deadlock.
        // onnegotiationneeded will fire again after we're stable and resend
        // anything we lost.
        if (peerConnection.signalingState !== 'stable') {
            console.warn('WebRTC offer collision, rolling back local offer');
            await peerConnection.setLocalDescription({ type: 'rollback' });
        }

        await peerConnection.setRemoteDescription(
            new RTCSessionDescription({ type: 'offer', sdp: payload.sdp })
        );
        flushPendingIce();

        const answer = await peerConnection.createAnswer();
        await peerConnection.setLocalDescription(answer);

        sendWS({
            type: 'webrtc_answer',
            payload: { sdp: answer.sdp },
        });
    } catch (err) {
        console.error('Failed to handle WebRTC offer:', err);
    }
}

function handleRemoteICECandidate(payload) {
    if (!peerConnection) return;
    // If remoteDescription isn't set yet, buffer the candidate.
    if (!peerConnection.remoteDescription) {
        pendingIceCandidates.push(payload.candidate);
        return;
    }
    peerConnection.addIceCandidate(new RTCIceCandidate(payload.candidate))
        .catch(err => console.error('Failed to add ICE candidate:', err));
}

// Adaptive publisher bitrate: monitors outbound-rtp stats and adjusts
// encoding maxBitrate in response to qualityLimitationReason. Returns a
// cleanup fn that stops the monitor. Logs transitions to the console.
function startAdaptiveBitrate(sender, tiers, label) {
    let tierIdx = 0;
    let lastGoodAt = Date.now();
    let stopped = false;

    const applyTier = async (idx) => {
        try {
            const params = sender.getParameters();
            if (!params.encodings || params.encodings.length === 0) {
                params.encodings = [{}];
            }
            params.encodings[0].maxBitrate = tiers[idx];
            await sender.setParameters(params);
            console.log('adaptive[' + label + ']: cap=' + Math.round(tiers[idx] / 1000) + ' kbps (tier ' + idx + ')');
        } catch (e) {
            // Sender may not be fully bound yet — just skip this tick.
        }
    };

    // Apply initial cap shortly after attach so the transport is ready.
    setTimeout(() => { if (!stopped) applyTier(0); }, 500);

    const interval = setInterval(async () => {
        if (stopped || !sender.track || sender.track.readyState !== 'live') return;
        let bwLimited = false;
        try {
            const stats = await sender.getStats();
            stats.forEach(s => {
                if (s.type === 'outbound-rtp' && s.kind === 'video' &&
                    s.qualityLimitationReason === 'bandwidth') {
                    bwLimited = true;
                }
            });
        } catch (e) {
            return;
        }
        const now = Date.now();
        if (bwLimited) {
            if (tierIdx < tiers.length - 1) {
                tierIdx++;
                await applyTier(tierIdx);
            }
            lastGoodAt = now;
        } else if (tierIdx > 0 && now - lastGoodAt > 15000) {
            tierIdx--;
            await applyTier(tierIdx);
            lastGoodAt = now;
        }
    }, 3000);

    return () => { stopped = true; clearInterval(interval); };
}

async function startScreenShare() {
    if (!peerConnection || isScreenSharing) return;

    try {
        screenStream = await navigator.mediaDevices.getDisplayMedia({
            video: { cursor: 'always' },
            audio: false,
        });

        const videoTrack = screenStream.getVideoTracks()[0];
        // Tell the SFU that the next video track is a screen, not a camera —
        // classification is flag-based on the server and the camera flag may
        // already be set from an earlier session restored from localStorage.
        sendWS({ type: 'screen_on' });
        // Per-track msid->kind mapping: unambiguous even when camera and
        // screen are added in the same renegotiation.
        sendWS({ type: 'media_track', payload: { stream_id: screenStream.id, kind: 'screen' } });
        screenSender = peerConnection.addTrack(videoTrack, screenStream);
        screenAdaptiveCleanup = startAdaptiveBitrate(screenSender, SCREEN_BITRATE_TIERS_BPS, 'screen');
        isScreenSharing = true;

        // When user stops sharing via browser UI
        videoTrack.onended = () => {
            stopScreenShare();
        };

        // Show local preview
        showLocalScreenPreview(screenStream);
        updateScreenShareUI();
        // onnegotiationneeded will handle the renegotiation

        // Start sending screen preview thumbnails
        setTimeout(captureAndSendPreview, 500);
        screenPreviewInterval = setInterval(captureAndSendPreview, 5 * 60 * 1000);
    } catch (err) {
        console.error('Screen share failed:', err);
    }
}

async function stopScreenShare() {
    if (!isScreenSharing) return;

    clearInterval(screenPreviewInterval);
    screenPreviewInterval = null;

    if (screenAdaptiveCleanup) { screenAdaptiveCleanup(); screenAdaptiveCleanup = null; }
    if (screenSender && peerConnection) {
        peerConnection.removeTrack(screenSender);
    }
    sendWS({ type: 'screen_off' });
    if (screenStream) {
        screenStream.getTracks().forEach(t => t.stop());
        screenStream = null;
    }
    screenSender = null;
    isScreenSharing = false;

    // onnegotiationneeded will handle renegotiation after removeTrack
    removeLocalScreenPreview();
    updateScreenShareUI();
}

// Add a screen-share tile to the camera grid. Shared by local and remote
// screen shares so they scale together with camera tiles. Screen tiles use
// col-span-2 to appear roughly twice as wide as camera tiles (more real estate
// for content) while still being part of the same responsive grid.
function addScreenTileToGrid({ id, stream, label, track }) {
    ensureCameraGrid();
    const grid = document.getElementById('camera-grid');
    if (!grid) return;

    // Replace existing tile if same id (e.g. on renegotiation)
    const existing = document.getElementById(id);
    if (existing) {
        const v = existing.querySelector('video');
        if (v) {
            v.srcObject = stream;
            v.play().catch(() => {});
        }
        return;
    }

    const wrapper = document.createElement('div');
    wrapper.id = id;
    wrapper.className = 'rounded-xl overflow-hidden bg-black border-2 border-vc-accent aspect-video relative group cursor-pointer col-span-2';
    wrapper.dataset.tileType = 'screen';

    const video = document.createElement('video');
    video.srcObject = stream;
    video.autoplay = true;
    video.playsInline = true;
    video.muted = true;
    // object-contain (not cover) so the full shared screen is visible
    video.className = 'w-full h-full object-contain';
    video.play().catch(() => {});

    const labelEl = document.createElement('div');
    labelEl.className = 'absolute bottom-2 left-2 bg-black/70 text-white text-xs px-2 py-1 rounded z-10';
    labelEl.textContent = label;

    const controls = document.createElement('div');
    controls.className = 'absolute top-1 right-1 flex gap-1 opacity-0 group-hover:opacity-100 transition z-10';
    controls.innerHTML = `
        <button onclick="event.stopPropagation(); setCamViewMode('${id}', 'expanded')" title="Expand" class="p-1 rounded bg-black/60 text-white hover:bg-white/20 transition">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 8V4m0 0h4M4 4l5 5m11-1V4m0 0h-4m4 0l-5 5M4 16v4m0 0h4m-4 0l5-5m11 5l-5-5m5 5v-4m0 4h-4"/></svg>
        </button>
        <button onclick="event.stopPropagation(); setCamViewMode('${id}', 'fullscreen')" title="Fullscreen" class="p-1 rounded bg-black/60 text-white hover:bg-white/20 transition">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4h6v2H6v4H4V4zm10 0h6v6h-2V6h-4V4zM6 14v4h4v2H4v-6h2zm12 4v-4h2v6h-6v-2h4z"/></svg>
        </button>
    `;

    wrapper.onclick = () => {
        const isExpanded = wrapper.dataset.expanded === 'true';
        setCamViewMode(id, isExpanded ? 'default' : 'expanded');
    };

    wrapper.appendChild(video);
    wrapper.appendChild(labelEl);
    wrapper.appendChild(controls);
    // Screen tiles always at the top of the grid
    grid.prepend(wrapper);
    updateGridColumns();

    if (track) {
        track.onended = () => removeScreenTileFromGrid(id);
    }
}

function removeScreenTileFromGrid(id) {
    const el = document.getElementById(id);
    if (el) {
        if (document.fullscreenElement === el) document.exitFullscreen().catch(() => {});
        el.remove();
        updateGridColumns();
    }
}

function showLocalScreenPreview(stream) {
    addScreenTileToGrid({ id: 'local-screen-share', stream, label: 'Your screen' });
}

function removeLocalScreenPreview() {
    removeScreenTileFromGrid('local-screen-share');
}

function showRemoteVideo(stream, track) {
    // Remote screen shares are rendered as grid tiles alongside cameras.
    // Using stream.id (stable from SFU: "screen-<userID>") as the tile id
    // ensures renegotiations replace the tile in place rather than duplicating.
    const id = 'remote-screen-share-' + stream.id;
    const label = screenShareUsername ? escapeHTML(screenShareUsername) + ' — screen' : 'Screen share';
    addScreenTileToGrid({ id, stream, label, track });
}

let screenViewMode = 'default';

function setScreenViewMode(mode) {
    const container = document.getElementById('screen-share-container');
    const video = document.getElementById('screen-share-video');
    if (!container || !video) return;

    // Exit fullscreen if leaving fullscreen mode
    if (screenViewMode === 'fullscreen' && mode !== 'fullscreen') {
        if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
    }

    screenViewMode = mode;

    // Remove close button if exists
    const oldCloseBtn = container.querySelector('.ss-close-btn');
    if (oldCloseBtn) oldCloseBtn.remove();

    if (mode === 'default') {
        container.className = 'w-full bg-black rounded-xl overflow-hidden mb-4 relative group';
        container.style.maxHeight = '';
        container.style.position = '';
        container.style.inset = '';
        container.style.zIndex = '';
        container.style.borderRadius = '';
        container.style.margin = '';
        video.className = 'w-full h-full object-contain';
        video.style.maxHeight = '70vh';
    } else if (mode === 'expanded') {
        container.className = 'bg-black overflow-hidden group';
        container.style.position = 'fixed';
        container.style.inset = '0';
        container.style.zIndex = '40';
        container.style.maxHeight = '';
        container.style.borderRadius = '0';
        container.style.margin = '0';
        // Add close button
        const closeBtn = document.createElement('button');
        closeBtn.className = 'ss-close-btn absolute top-4 right-4 z-20 p-2 rounded-lg bg-black/70 text-white hover:bg-white/20 transition';
        closeBtn.title = 'Exit (Esc)';
        closeBtn.innerHTML = '<svg class="w-6 h-6" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/></svg>';
        closeBtn.onclick = () => setScreenViewMode('default');
        container.appendChild(closeBtn);
        video.className = 'w-full h-full object-contain';
        video.style.maxHeight = '';
    } else if (mode === 'fullscreen') {
        container.className = 'w-full bg-black rounded-xl overflow-hidden mb-4 relative group';
        container.style.position = '';
        container.style.inset = '';
        container.style.zIndex = '';
        container.style.borderRadius = '';
        container.style.margin = '';
        video.className = 'w-full h-full object-contain';
        video.style.maxHeight = '';
        if (container.requestFullscreen) {
            container.requestFullscreen().catch(() => {});
        }
    }
}

// Handle ESC from fullscreen
document.addEventListener('fullscreenchange', () => {
    if (!document.fullscreenElement && screenViewMode === 'fullscreen') {
        setScreenViewMode('default');
    }
});
document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && screenViewMode === 'expanded') {
        setScreenViewMode('default');
    }
});

function removeRemoteVideo() {
    // Remove all remote-screen-share tiles from the grid.
    const grid = document.getElementById('camera-grid');
    if (!grid) return;
    grid.querySelectorAll('[id^="remote-screen-share-"]').forEach(el => {
        if (document.fullscreenElement === el) document.exitFullscreen().catch(() => {});
        el.remove();
    });
    updateGridColumns();
}

function updateScreenShareUI() {
    const btn = document.getElementById('screen-share-btn');
    if (!btn) return;
    if (isScreenSharing) {
        btn.className = 'flex items-center gap-2 px-4 py-2 rounded-lg bg-vc-green/20 text-vc-green transition';
        btn.innerHTML = `
            <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z"/>
            </svg>
            <span>Stop Sharing</span>`;
    } else {
        btn.className = 'flex items-center gap-2 px-4 py-2 rounded-lg bg-vc-channel hover:bg-vc-hover text-vc-text transition';
        btn.innerHTML = `
            <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9.75 17L9 20l-1 1h8l-1-1-.75-3M3 13h18M5 17h14a2 2 0 002-2V5a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z"/>
            </svg>
            <span>Share Screen</span>`;
    }
}

// ─── Camera ───────────────────────────────────────────────────

async function toggleCamera() {
    if (isCameraOn) {
        stopCamera();
    } else {
        await startCamera();
    }
    localStorage.setItem('vocala-camera', isCameraOn);
}

async function startCamera() {
    if (!peerConnection) return;
    try {
        const camConstraints = { width: { ideal: 640 }, height: { ideal: 480 }, facingMode: 'user' };
        if (selectedCamId) camConstraints.deviceId = { exact: selectedCamId };
        cameraStream = await navigator.mediaDevices.getUserMedia({
            video: camConstraints,
            audio: false,
        });

        const videoTrack = cameraStream.getVideoTracks()[0];

        // Tell SFU next video track is camera, then add track
        // SFU will initiate renegotiation — do NOT create offer from client
        sendWS({ type: 'camera_on' });
        sendWS({ type: 'media_track', payload: { stream_id: cameraStream.id, kind: 'camera' } });
        cameraSender = peerConnection.addTrack(videoTrack, cameraStream);
        cameraAdaptiveCleanup = startAdaptiveBitrate(cameraSender, CAMERA_BITRATE_TIERS_BPS, 'camera');

        isCameraOn = true;
        updateCameraUI();
        addLocalCameraToGrid();

        videoTrack.onended = () => stopCamera();
    } catch (err) {
        console.error('Failed to start camera:', err);
        // Make sure expectCamera flag is cleared on server
        sendWS({ type: 'camera_off' });
    }
}

function stopCamera() {
    if (cameraAdaptiveCleanup) { cameraAdaptiveCleanup(); cameraAdaptiveCleanup = null; }
    if (cameraStream) {
        cameraStream.getTracks().forEach(t => t.stop());
        cameraStream = null;
    }
    sendWS({ type: 'camera_off' });
    if (cameraSender && peerConnection) {
        peerConnection.removeTrack(cameraSender);
        cameraSender = null;
    }
    isCameraOn = false;
    updateCameraUI();
    removeFromCameraGrid('local-camera');
}

function updateCameraUI() {
    const btn = document.getElementById('camera-btn');
    if (!btn) return;
    btn.className = isCameraOn
        ? 'flex items-center gap-1.5 px-3 py-2 rounded-lg bg-vc-green/20 text-vc-green transition text-sm'
        : 'flex items-center gap-1.5 px-3 py-2 rounded-lg bg-vc-channel hover:bg-vc-hover text-vc-text transition text-sm';
}

// --- Unified camera grid (local + remote) ---

function ensureCameraGrid() {
    if (document.getElementById('camera-grid')) return;
    const anchor = document.getElementById('screen-share-anchor');
    if (!anchor) return;

    const grid = document.createElement('div');
    grid.id = 'camera-grid';
    grid.className = 'grid gap-3 mb-4 w-full max-w-5xl mx-auto';
    anchor.parentElement.insertBefore(grid, anchor.nextSibling);
    updateGridColumns();
}

function updateGridColumns() {
    const grid = document.getElementById('camera-grid');
    if (!grid) return;
    const count = grid.children.length;
    let cls;
    if (count <= 1) {
        cls = 'grid grid-cols-1 gap-3 mb-4 w-full max-w-2xl mx-auto';
    } else if (count === 2) {
        cls = 'grid grid-cols-1 md:grid-cols-2 gap-3 mb-4 w-full max-w-4xl mx-auto';
    } else if (count <= 4) {
        cls = 'grid grid-cols-2 gap-3 mb-4 w-full max-w-5xl mx-auto';
    } else if (count <= 9) {
        cls = 'grid grid-cols-2 md:grid-cols-3 gap-2 mb-4 w-full max-w-6xl mx-auto';
    } else if (count <= 16) {
        cls = 'grid grid-cols-3 md:grid-cols-4 gap-2 mb-4 w-full mx-auto';
    } else {
        cls = 'grid grid-cols-3 md:grid-cols-4 lg:grid-cols-5 gap-1.5 mb-4 w-full mx-auto';
    }
    grid.className = cls;
}

function addLocalCameraToGrid() {
    ensureCameraGrid();
    const grid = document.getElementById('camera-grid');
    if (!grid || document.getElementById('local-camera')) return;

    const wrapper = document.createElement('div');
    wrapper.id = 'local-camera';
    wrapper.className = 'rounded-xl overflow-hidden bg-black border-2 border-vc-accent aspect-video relative group cursor-pointer';

    const video = document.createElement('video');
    video.srcObject = cameraStream;
    video.autoplay = true;
    video.muted = true;
    video.playsInline = true;
    video.className = 'w-full h-full object-cover';
    video.style.transform = 'scaleX(-1)';

    const label = document.createElement('div');
    label.className = 'absolute bottom-2 left-2 bg-black/60 text-white text-xs px-2 py-0.5 rounded z-10';
    label.textContent = 'You';

    const localControls = document.createElement('div');
    localControls.className = 'absolute top-1 right-1 flex gap-1 opacity-0 group-hover:opacity-100 transition z-10';
    localControls.innerHTML = `
        <button onclick="event.stopPropagation(); setCamViewMode('local-camera', 'expanded')" title="Expand" class="p-1 rounded bg-black/60 text-white hover:bg-white/20 transition">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 8V4m0 0h4M4 4l5 5m11-1V4m0 0h-4m4 0l-5 5M4 16v4m0 0h4m-4 0l5-5m11 5l-5-5m5 5v-4m0 4h-4"/></svg>
        </button>
        <button onclick="event.stopPropagation(); setCamViewMode('local-camera', 'fullscreen')" title="Fullscreen" class="p-1 rounded bg-black/60 text-white hover:bg-white/20 transition">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4h6v2H6v4H4V4zm10 0h6v6h-2V6h-4V4zM6 14v4h4v2H4v-6h2zm12 4v-4h2v6h-6v-2h4z"/></svg>
        </button>
    `;

    wrapper.onclick = () => {
        const isExpanded = wrapper.dataset.expanded === 'true';
        setCamViewMode('local-camera', isExpanded ? 'default' : 'expanded');
    };

    wrapper.appendChild(video);
    wrapper.appendChild(label);
    wrapper.appendChild(localControls);
    // Local camera always first
    grid.prepend(wrapper);
    updateGridColumns();
}

function handleRemoteCameraTrack(stream, track, mid) {
    ensureCameraGrid();
    const grid = document.getElementById('camera-grid');
    if (!grid) return;

    // stream.id = "camera-{userID}" from SFU — stable across renegotiation
    const camId = 'remote-cam-' + stream.id;
    console.log('Camera track:', { mid, trackId: track.id, streamId: stream.id, camId });
    const existing = document.getElementById(camId);
    if (existing) {
        const video = existing.querySelector('video');
        if (video) {
            video.srcObject = stream;
            video.play().catch(() => {});
        }
        return;
    }

    const wrapper = document.createElement('div');
    wrapper.id = camId;
    wrapper.className = 'rounded-xl overflow-hidden bg-black border border-vc-border aspect-video relative group cursor-pointer';

    const video = document.createElement('video');
    video.srcObject = stream;
    video.autoplay = true;
    video.playsInline = true;
    video.muted = true;
    video.className = 'w-full h-full object-cover';
    video.play().catch(() => {});

    // View mode controls (hover)
    const controls = document.createElement('div');
    controls.className = 'absolute top-1 right-1 flex gap-1 opacity-0 group-hover:opacity-100 transition z-10';
    controls.innerHTML = `
        <button onclick="event.stopPropagation(); setCamViewMode('${camId}', 'expanded')" title="Expand" class="p-1 rounded bg-black/60 text-white hover:bg-white/20 transition">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 8V4m0 0h4M4 4l5 5m11-1V4m0 0h-4m4 0l-5 5M4 16v4m0 0h4m-4 0l5-5m11 5l-5-5m5 5v-4m0 4h-4"/></svg>
        </button>
        <button onclick="event.stopPropagation(); setCamViewMode('${camId}', 'fullscreen')" title="Fullscreen" class="p-1 rounded bg-black/60 text-white hover:bg-white/20 transition">
            <svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4h6v2H6v4H4V4zm10 0h6v6h-2V6h-4V4zM6 14v4h4v2H4v-6h2zm12 4v-4h2v6h-6v-2h4z"/></svg>
        </button>
    `;

    // Click on video to toggle expand
    wrapper.onclick = () => {
        const isExpanded = wrapper.dataset.expanded === 'true';
        setCamViewMode(camId, isExpanded ? 'default' : 'expanded');
    };

    wrapper.appendChild(video);
    wrapper.appendChild(controls);
    grid.appendChild(wrapper);
    updateGridColumns();

    track.onended = () => removeFromCameraGrid(camId);

    let muteTimer = null;
    track.onmute = () => {
        muteTimer = setTimeout(() => removeFromCameraGrid(camId), 5000);
    };
    track.onunmute = () => {
        if (muteTimer) { clearTimeout(muteTimer); muteTimer = null; }
    };
}

let expandedCamId = null;

function setCamViewMode(camId, mode) {
    const wrapper = document.getElementById(camId);
    if (!wrapper) return;

    // Reset previous expanded
    if (expandedCamId && expandedCamId !== camId) {
        const prev = document.getElementById(expandedCamId);
        if (prev) {
            prev.style.position = '';
            prev.style.inset = '';
            prev.style.zIndex = '';
            prev.style.borderRadius = '';
            prev.style.width = '';
            prev.style.height = '';
            prev.dataset.expanded = '';
            prev.className = prev.className.replace('fixed', '').trim();
        }
        expandedCamId = null;
    }

    if (mode === 'default') {
        wrapper.style.position = '';
        wrapper.style.inset = '';
        wrapper.style.zIndex = '';
        wrapper.style.borderRadius = '';
        wrapper.style.width = '';
        wrapper.style.height = '';
        wrapper.dataset.expanded = '';
        expandedCamId = null;
        const video = wrapper.querySelector('video');
        if (video) video.className = 'w-full h-full object-cover';
        const closeBtn = wrapper.querySelector('.cam-close-btn');
        if (closeBtn) closeBtn.remove();
    } else if (mode === 'expanded') {
        wrapper.style.position = 'fixed';
        wrapper.style.inset = '0';
        wrapper.style.zIndex = '40';
        wrapper.style.borderRadius = '0';
        wrapper.style.width = '100vw';
        wrapper.style.height = '100vh';
        wrapper.dataset.expanded = 'true';
        expandedCamId = camId;
        const video = wrapper.querySelector('video');
        if (video) video.className = 'w-full h-full object-contain';
        // Add close button (always visible)
        let closeBtn = wrapper.querySelector('.cam-close-btn');
        if (!closeBtn) {
            closeBtn = document.createElement('button');
            closeBtn.className = 'cam-close-btn absolute top-4 right-4 z-20 p-2 rounded-lg bg-black/70 text-white hover:bg-white/20 transition';
            closeBtn.title = 'Exit (Esc)';
            closeBtn.innerHTML = '<svg class="w-6 h-6" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/></svg>';
            closeBtn.onclick = (e) => { e.stopPropagation(); setCamViewMode(camId, 'default'); };
            wrapper.appendChild(closeBtn);
        }
    } else if (mode === 'fullscreen') {
        wrapper.dataset.expanded = '';
        expandedCamId = null;
        wrapper.style.position = '';
        wrapper.style.inset = '';
        wrapper.style.zIndex = '';
        wrapper.style.borderRadius = '';
        wrapper.style.width = '';
        wrapper.style.height = '';
        if (wrapper.requestFullscreen) {
            wrapper.requestFullscreen().catch(() => {});
        }
    }
}

// ESC to exit expanded camera
document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && expandedCamId) {
        setCamViewMode(expandedCamId, 'default');
    }
});

function removeFromCameraGrid(id) {
    const el = document.getElementById(id);
    if (el) el.remove();
    const grid = document.getElementById('camera-grid');
    if (grid && grid.children.length === 0) {
        grid.remove();
    } else {
        updateGridColumns();
    }
}

function showScreenPreviewPlaceholder() {
    // No-op: remote screen shares are now rendered as tiles in camera-grid
    // via showRemoteVideo() as soon as the video track arrives. The standalone
    // preview placeholder is obsolete in the unified-grid layout.
}

function captureAndSendPreview() {
    if (!screenStream) return;
    const video = document.querySelector('#local-screen-share video');
    if (!video || !video.videoWidth) return;
    const canvas = document.createElement('canvas');
    canvas.width = 320;
    canvas.height = Math.round(320 * video.videoHeight / video.videoWidth);
    const ctx = canvas.getContext('2d');
    ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
    const dataUrl = canvas.toDataURL('image/jpeg', 0.5);
    sendWS({ type: 'screen_preview', payload: { image: dataUrl } });
}

function updateScreenPreviewOverlay() {
    // No-op (see showScreenPreviewPlaceholder).
}

function cleanupWebRTC() {
    const caller = (new Error().stack || '').split('\n')[2] || '?';
    console.log('cleanupWebRTC: stopping all media (called from ' + caller.trim() + ')');
    pendingIceCandidates = [];
    clearInterval(screenPreviewInterval);
    screenPreviewInterval = null;
    latestScreenPreview = null;
    screenShareUsername = null;
    if (vadInterval) {
        clearInterval(vadInterval);
        vadInterval = null;
    }
    if (audioContext) {
        audioContext.close().catch(() => {});
        audioContext = null;
        analyser = null;
    }
    if (screenAdaptiveCleanup) { screenAdaptiveCleanup(); screenAdaptiveCleanup = null; }
    if (screenStream) {
        screenStream.getTracks().forEach(t => t.stop());
        screenStream = null;
    }
    screenSender = null;
    isScreenSharing = false;
    removeRemoteVideo();
    removeLocalScreenPreview();
    // Cleanup camera
    if (cameraAdaptiveCleanup) { cameraAdaptiveCleanup(); cameraAdaptiveCleanup = null; }
    if (cameraStream) {
        cameraStream.getTracks().forEach(t => t.stop());
        cameraStream = null;
    }
    cameraSender = null;
    isCameraOn = false;
    remoteCameras = {};
    const cameraGrid = document.getElementById('camera-grid');
    if (cameraGrid) cameraGrid.remove();
    if (peerConnection) {
        peerConnection.close();
        peerConnection = null;
    }
    if (processedStream) {
        processedStream.getTracks().forEach(t => t.stop());
        processedStream = null;
    }
    if (localStream) {
        localStream.getTracks().forEach(t => t.stop());
        localStream = null;
    }
    gainNode = null;
    isSpeaking = false;
}

function updateRTCStatus() {
    if (!peerConnection) return;
    const state = peerConnection.connectionState || peerConnection.iceConnectionState;
    switch (state) {
        case 'connected':
        case 'completed':
            updateRTCStatusText('connected', 'Voice connected');
            break;
        case 'connecting':
        case 'checking':
        case 'new':
            updateRTCStatusText('connecting', 'Connecting...');
            break;
        case 'disconnected':
            updateRTCStatusText('warning', 'Disconnected');
            break;
        case 'failed':
            updateRTCStatusText('error', 'Connection failed');
            break;
        case 'closed':
            updateRTCStatusText('error', 'Closed');
            break;
    }
}

function updateRTCStatusText(state, text) {
    const el = document.getElementById('rtc-status');
    if (!el) return;

    const colors = {
        connected: { dot: 'bg-vc-green', text: 'text-vc-green', pulse: '' },
        connecting: { dot: 'bg-vc-yellow', text: 'text-vc-yellow', pulse: 'animate-pulse' },
        warning: { dot: 'bg-vc-yellow', text: 'text-vc-yellow', pulse: '' },
        error: { dot: 'bg-vc-red', text: 'text-vc-red', pulse: '' },
    };
    const c = colors[state] || colors.error;
    el.innerHTML = `
        <div class="w-2 h-2 rounded-full ${c.dot} ${c.pulse}"></div>
        <span class="text-xs ${c.text}">${text}</span>
    `;
}

// ─── Voice Activity Detection ─────────────────────────────────

function setupVAD(stream) {
    // audioContext is already created in startWebRTC
    analyser = audioContext.createAnalyser();
    analyser.fftSize = 512;
    analyser.smoothingTimeConstant = 0.3;

    const source = audioContext.createMediaStreamSource(stream);
    source.connect(analyser);

    const dataArray = new Uint8Array(analyser.frequencyBinCount);
    let silenceCount = 0;
    const SILENCE_DELAY = 5; // ~250ms at 50ms intervals

    vadInterval = setInterval(() => {
        analyser.getByteFrequencyData(dataArray);
        let sum = 0;
        for (let i = 0; i < dataArray.length; i++) {
            sum += dataArray[i];
        }
        currentVadLevel = sum / dataArray.length;

        // Update level meter
        const meter = document.getElementById('vad-meter');
        if (meter) {
            const pct = Math.min(100, (currentVadLevel / 80) * 100);
            meter.style.width = pct + '%';
            meter.className = `h-full rounded-full transition-all duration-75 ${currentVadLevel > vadThreshold ? 'bg-vc-green' : 'bg-vc-muted/50'}`;
        }

        if (isMuted || (pushToTalk && !pttActive)) return;

        const voiceDetected = currentVadLevel > vadThreshold;

        if (voiceDetected) {
            silenceCount = 0;
            if (gainNode) gainNode.gain.value = 1.0;
            if (!isSpeaking) {
                isSpeaking = true;
                sendWS({ type: 'speaking', payload: { speaking: true } });
                updateSelfSpeakingUI(true);
            }
        } else {
            silenceCount++;
            if (silenceCount >= SILENCE_DELAY) {
                if (gainNode && !pushToTalk) gainNode.gain.value = 0.0;
                if (isSpeaking) {
                    isSpeaking = false;
                    sendWS({ type: 'speaking', payload: { speaking: false } });
                    updateSelfSpeakingUI(false);
                }
            }
        }
    }, 50);
}

function updateSelfSpeakingUI(speaking) {
    const avatar = document.getElementById('self-avatar');
    const indicator = document.getElementById('self-speaking-indicator');
    if (avatar) {
        if (speaking) {
            avatar.classList.add('ring-2', 'ring-vc-green', 'ring-offset-1', 'ring-offset-vc-bg');
        } else {
            avatar.classList.remove('ring-2', 'ring-vc-green', 'ring-offset-1', 'ring-offset-vc-bg');
        }
    }
    if (indicator) {
        indicator.classList.toggle('hidden', !speaking);
        indicator.classList.toggle('flex', speaking);
    }
}

function setVadThreshold(value) {
    vadThreshold = parseInt(value);
    localStorage.setItem('vocala-vad-threshold', vadThreshold);
    const label = document.getElementById('vad-threshold-label');
    if (label) label.textContent = vadThreshold;
    const marker = document.getElementById('vad-threshold-marker');
    if (marker) marker.style.left = Math.min(100, (vadThreshold / 80) * 100) + '%';
}

// ─── Push-to-Talk Keyboard ────────────────────────────────────

document.addEventListener('keydown', (e) => {
    if (!pushToTalk || !localStream) return;
    if (e.code === 'Space' && !e.repeat && !isInputFocused()) {
        e.preventDefault();
        pttActive = true;
        if (gainNode) gainNode.gain.value = 1.0;
    }
});

document.addEventListener('keyup', (e) => {
    if (!pushToTalk || !localStream) return;
    if (e.code === 'Space' && !isInputFocused()) {
        e.preventDefault();
        pttActive = false;
        if (gainNode) gainNode.gain.value = 0.0;
    }
});

function isInputFocused() {
    const el = document.activeElement;
    return el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.contentEditable === 'true');
}

// ─── Init ─────────────────────────────────────────────────────

// Delegated click handlers for data-action buttons (XSS-safe, no inline onclick)
document.addEventListener('click', function(e) {
    const btn = e.target.closest('[data-action]');
    if (!btn) return;
    const action = btn.dataset.action;
    const chId = +btn.dataset.chId;
    const chName = btn.dataset.chName;
    if (action === 'manage-members') openMemberManager(chId, chName);
    if (action === 'delete-channel') deleteChannel(chId, chName);
    if (action === 'toggle-privacy') toggleChannelPrivacy(chId, chName, btn.dataset.private === 'true');
    if (action === 'join-channel') joinChannel(chId, chName);
});

// Set self avatar
const selfAvatar = document.getElementById('self-avatar');
if (selfAvatar && selfAvatar.dataset.username) {
    selfAvatar.src = avatarURL(selfAvatar.dataset.username);
}

connectWS();
checkMicPermission();
// Notification.requestPermission() must be called from a user gesture —
// Firefox blocks it otherwise. Defer to the first click on the page.
document.addEventListener('click', requestNotificationPermission, { once: true, capture: true });

async function checkMicPermission() {
    try {
        const result = await navigator.permissions.query({ name: 'microphone' });
        if (result.state === 'denied') {
            showGlobalMicWarning();
        } else if (result.state === 'prompt') {
            // Proactively request mic access so the browser shows the permission prompt
            try {
                const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
                stream.getTracks().forEach(t => t.stop());
            } catch (e) {
                showGlobalMicWarning();
            }
        }
        result.addEventListener('change', () => {
            if (result.state === 'denied') {
                showGlobalMicWarning();
            } else {
                hideGlobalMicWarning();
            }
        });
    } catch (e) {
        // permissions.query not supported, try getUserMedia directly
        try {
            const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
            stream.getTracks().forEach(t => t.stop());
        } catch (err) {
            showGlobalMicWarning();
        }
    }
}

function showGlobalMicWarning() {
    if (!isMuted) {
        isMuted = true;
        updateMuteUI();
    }
    if (document.getElementById('global-mic-warning')) return;
    const banner = document.createElement('div');
    banner.id = 'global-mic-warning';
    banner.className = 'fixed top-0 left-0 right-0 z-50 bg-vc-red/90 backdrop-blur-sm text-white px-4 py-3 flex items-center justify-center gap-3 text-sm shadow-lg';
    banner.innerHTML = `
        <svg class="w-5 h-5 flex-shrink-0" fill="currentColor" viewBox="0 0 24 24">
            <path d="M19 11h-1.7c0 .74-.16 1.43-.43 2.05l1.23 1.23c.56-.98.9-2.09.9-3.28zm-4.02.17c0-.06.02-.11.02-.17V5c0-1.66-1.34-3-3-3S9 3.34 9 5v.18l5.98 5.99zM4.27 3L3 4.27l6.01 6.01V11c0 1.66 1.33 3 2.99 3 .22 0 .44-.03.65-.08l1.66 1.66c-.71.33-1.5.52-2.31.52-2.76 0-5.3-2.1-5.3-5.1H5c0 3.41 2.72 6.23 6 6.72V21h2v-3.28c.91-.13 1.77-.45 2.54-.9L19.73 21 21 19.73 4.27 3z"/>
        </svg>
        <span><strong>Microphone blocked</strong> — Click the lock icon in the address bar, allow microphone access, and reload the page.</span>
    `;
    document.body.prepend(banner);
}

function hideGlobalMicWarning() {
    const banner = document.getElementById('global-mic-warning');
    if (banner) banner.remove();
}

// ─── Chat ─────────────────────────────────────────────────────

const REACTION_EMOJIS = ['👍', '❤️', '😂', '😮', '👏', '🔥'];

function clearChat() {
    if (!confirm('Clear all chat messages in this channel?')) return;
    sendWS({ type: 'clear_chat' });
}

function sendChatMessage(event) {
    event.preventDefault();
    const input = document.getElementById('chat-input');
    const text = input.value.trim();
    if (!text || !currentChannelID) return;

    sendWS({
        type: 'chat_message',
        payload: { text },
    });
    input.value = '';
}

function loadChatHistory(messages) {
    const container = document.getElementById('chat-messages');
    if (!container) return;
    container.innerHTML = '';
    messages.forEach(msg => appendChatMessage({ ...msg, _history: true }));
}

function appendChatMessage(msg) {
    const container = document.getElementById('chat-messages');
    if (!container) return;

    // Sound + notification for messages from others (not history load)
    const selfName = document.getElementById('self-avatar')?.dataset?.username;
    if (msg.username !== selfName && !msg._history) {
        playChatSound();
        if (document.hidden) showNotification(msg.username + ': ' + msg.text);
    }

    const el = document.createElement('div');
    el.id = 'msg-' + msg.id;
    el.className = 'group relative px-2 py-1 rounded hover:bg-vc-hover/30 transition';

    const time = new Date(msg.timestamp * 1000);
    const timeStr = time.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

    el.innerHTML = `
        <div class="flex gap-2">
            <img src="${avatarURL(msg.username)}" alt="" class="w-6 h-6 rounded-full flex-shrink-0 mt-0.5">
            <div class="min-w-0 flex-1">
                <div class="flex items-baseline gap-1.5">
                    <span class="text-xs font-semibold text-vc-accent">${escapeHTML(msg.username)}</span>
                    <span class="text-[10px] text-vc-muted">${timeStr}</span>
                </div>
                <div class="text-sm text-vc-text break-words">${escapeHTML(msg.text)}</div>
                <div class="reactions flex flex-wrap gap-1 mt-0.5" id="reactions-${msg.id}"></div>
            </div>
        </div>
        <button onclick="showReactionPicker('${msg.id}')"
            class="absolute right-1 top-0.5 opacity-0 group-hover:opacity-100 text-xs bg-vc-channel hover:bg-vc-hover rounded px-1 py-0.5 transition text-vc-muted">
            +
        </button>
    `;

    container.appendChild(el);
    container.scrollTop = container.scrollHeight;
}

function showReactionPicker(messageId) {
    // Remove existing picker
    const old = document.getElementById('reaction-picker');
    if (old) old.remove();

    const picker = document.createElement('div');
    picker.id = 'reaction-picker';
    picker.className = 'absolute z-50 bg-vc-sidebar border border-vc-border rounded-lg shadow-lg p-1 flex gap-0.5';

    REACTION_EMOJIS.forEach(emoji => {
        const btn = document.createElement('button');
        btn.className = 'hover:bg-vc-hover rounded p-1 text-sm transition';
        btn.textContent = emoji;
        btn.onclick = () => {
            sendWS({
                type: 'chat_reaction',
                payload: { message_id: messageId, emoji },
            });
            picker.remove();
        };
        picker.appendChild(btn);
    });

    const msgEl = document.getElementById('msg-' + messageId);
    if (msgEl) {
        msgEl.appendChild(picker);
        // Auto-remove after 5s
        setTimeout(() => picker.remove(), 5000);
    }
}

function addChatReaction(msg) {
    const container = document.getElementById('reactions-' + msg.message_id);
    if (!container) return;

    const userId = String(msg.user_id);
    const existing = container.querySelector(`[data-emoji="${msg.emoji}"]`);
    if (existing) {
        const users = (existing.dataset.users || '').split(',').filter(Boolean);
        if (users.includes(userId)) return;
        users.push(userId);
        existing.dataset.users = users.join(',');
        existing.dataset.count = String(users.length);
        existing.textContent = msg.emoji + (users.length > 1 ? ' ' + users.length : '');
        return;
    }

    const badge = document.createElement('span');
    badge.className = 'inline-flex items-center px-1 py-0.5 rounded bg-vc-channel text-xs cursor-default';
    badge.dataset.emoji = msg.emoji;
    badge.dataset.users = userId;
    badge.dataset.count = '1';
    badge.textContent = msg.emoji;
    badge.title = msg.username;
    container.appendChild(badge);
}

// ─── WS Media Transport (mobile fallback) ─────────────────────

const USE_WS_MEDIA = /Android|iPhone|iPad|iPod/i.test(navigator.userAgent);
let wsMediaRecorder = null;
let wsMediaAudioElements = {}; // userID -> Audio element
let wsMediaVideoElements = {}; // userID -> container element

async function startWSMedia() {
    try {
        localStream = await navigator.mediaDevices.getUserMedia({
            audio: { echoCancellation: true, noiseSuppression: true, autoGainControl: true },
            video: false,
        });

        // Setup VAD (reads raw stream for level detection)
        audioContext = new AudioContext();
        const source = audioContext.createMediaStreamSource(localStream);
        gainNode = audioContext.createGain();
        gainNode.gain.value = (pushToTalk || isMuted) ? 0.0 : 1.0;
        const dest = audioContext.createMediaStreamDestination();
        source.connect(gainNode);
        gainNode.connect(dest);
        processedStream = dest.stream;
        setupVAD(localStream);

        // Tell server we use WS media
        sendWS({ type: 'ws_media_mode' });

        // Start recording processed audio and sending via WS
        startWSAudioSend(processedStream);

        // Handle incoming binary frames
        ws.binaryType = 'arraybuffer';

        updateRTCStatus();
        const statusEl = document.getElementById('rtc-status');
        if (statusEl) {
            statusEl.innerHTML = '<div class="w-2 h-2 rounded-full bg-vc-green"></div><span class="text-xs text-vc-green">Connected (WS)</span>';
        }
    } catch (err) {
        console.error('WS Media failed:', err);
        showGlobalMicWarning();
    }
}

function startWSAudioSend(stream) {
    // Use MediaRecorder with small timeslice for low latency
    const mimeType = MediaRecorder.isTypeSupported('audio/webm;codecs=opus')
        ? 'audio/webm;codecs=opus'
        : 'audio/webm';

    wsMediaRecorder = new MediaRecorder(stream, {
        mimeType: mimeType,
        audioBitsPerSecond: 32000,
    });

    wsMediaRecorder.ondataavailable = (event) => {
        if (event.data.size > 0 && ws && ws.readyState === WebSocket.OPEN) {
            event.data.arrayBuffer().then(buf => {
                // Frame: [0x01 (audio)] + [payload]
                const frame = new Uint8Array(1 + buf.byteLength);
                frame[0] = 0x01;
                frame.set(new Uint8Array(buf), 1);
                ws.send(frame.buffer);
            });
        }
    };

    wsMediaRecorder.start(60); // 60ms chunks for low latency
}

function stopWSMedia() {
    if (wsMediaRecorder && wsMediaRecorder.state !== 'inactive') {
        wsMediaRecorder.stop();
        wsMediaRecorder = null;
    }
    // Clean up remote audio elements
    Object.values(wsMediaAudioElements).forEach(el => {
        if (el.src) URL.revokeObjectURL(el.src);
        el.remove();
    });
    wsMediaAudioElements = {};
    Object.values(wsMediaVideoElements).forEach(el => el.remove());
    wsMediaVideoElements = {};
}

function handleWSMediaFrame(data) {
    const view = new DataView(data);
    if (data.byteLength < 10) return;

    const type = view.getUint8(0);
    const userIdHi = view.getUint32(1);
    const userIdLo = view.getUint32(5);
    const userId = userIdHi * 0x100000000 + userIdLo;
    const payload = data.slice(9);

    if (type === 0x01) {
        // Audio frame
        playWSAudio(userId, payload);
    } else if (type === 0x02) {
        // Video frame (future)
        playWSVideo(userId, payload);
    }
}

// Audio playback using MediaSource or Blob URLs
function playWSAudio(userId, payload) {
    if (!wsMediaAudioElements[userId]) {
        const audio = new Audio();
        audio.autoplay = true;
        wsMediaAudioElements[userId] = audio;
    }

    const audio = wsMediaAudioElements[userId];
    const blob = new Blob([payload], { type: 'audio/webm;codecs=opus' });
    const url = URL.createObjectURL(blob);
    
    // Queue playback
    if (!audio._queue) audio._queue = [];
    audio._queue.push(url);

    if (audio.paused || audio.ended) {
        playNextChunk(audio);
    }
}

function playNextChunk(audio) {
    if (!audio._queue || audio._queue.length === 0) return;
    
    const url = audio._queue.shift();
    if (audio._prevUrl) URL.revokeObjectURL(audio._prevUrl);
    audio._prevUrl = url;
    audio.src = url;
    audio.play().catch(() => {});
    audio.onended = () => playNextChunk(audio);
}

function playWSVideo(userId, payload) {
    // Placeholder for future video support
}

// WS camera send
let wsCameraRecorder = null;

async function startWSCamera() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    try {
        cameraStream = await navigator.mediaDevices.getUserMedia({
            video: { width: { ideal: 640 }, height: { ideal: 480 }, facingMode: 'user' },
            audio: false,
        });

        const mimeType = MediaRecorder.isTypeSupported('video/webm;codecs=vp8')
            ? 'video/webm;codecs=vp8'
            : 'video/webm';

        wsCameraRecorder = new MediaRecorder(cameraStream, {
            mimeType: mimeType,
            videoBitsPerSecond: 500000,
        });

        wsCameraRecorder.ondataavailable = (event) => {
            if (event.data.size > 0 && ws && ws.readyState === WebSocket.OPEN) {
                event.data.arrayBuffer().then(buf => {
                    // Frame: [0x02 (video)] + [payload]
                    const frame = new Uint8Array(1 + buf.byteLength);
                    frame[0] = 0x02;
                    frame.set(new Uint8Array(buf), 1);
                    ws.send(frame.buffer);
                });
            }
        };

        wsCameraRecorder.start(100); // 100ms chunks

        isCameraOn = true;
        updateCameraUI();
        addLocalCameraToGrid();

        cameraStream.getVideoTracks()[0].onended = () => stopWSCamera();
    } catch (err) {
        console.error('WS Camera failed:', err);
    }
}

function stopWSCamera() {
    if (wsCameraRecorder && wsCameraRecorder.state !== 'inactive') {
        wsCameraRecorder.stop();
        wsCameraRecorder = null;
    }
    if (cameraStream) {
        cameraStream.getTracks().forEach(t => t.stop());
        cameraStream = null;
    }
    isCameraOn = false;
    updateCameraUI();
    removeFromCameraGrid('local-camera');
}

// ─── Auto-join from URL ───────────────────────────────────────

function autoJoinFromURL() {
    const channelName = window.VOCALA_AUTO_JOIN;
    if (!channelName) return;

    const tryJoin = () => {
        const buttons = document.querySelectorAll('[data-ch-name]');
        for (const btn of buttons) {
            if (btn.dataset.chName === channelName) {
                const chId = parseInt(btn.dataset.chId);
                joinChannel(chId, channelName);
                return true;
            }
        }
        return false;
    };

    if (!tryJoin()) {
        setTimeout(tryJoin, 1000);
    }
}

// Handle browser back/forward
window.addEventListener('popstate', (event) => {
    if (event.state && event.state.channelID) {
        joinChannel(event.state.channelID, event.state.channelName);
    } else if (currentChannelID) {
        leaveChannel();
    }
});

// --- Private channel member management ---

function getCSRFToken() {
    const match = document.cookie.match(/csrf_token=([^;]+)/);
    return match ? match[1] : '';
}

async function openMemberManager(channelId, channelName) {
    // Remove existing modal
    const old = document.getElementById('member-modal');
    if (old) old.remove();

    const modal = document.createElement('div');
    modal.id = 'member-modal';
    modal.className = 'fixed inset-0 z-50 flex items-center justify-center bg-black/60';
    modal.innerHTML = `
        <div class="bg-vc-sidebar border border-vc-border rounded-xl shadow-2xl w-96 max-h-[80vh] flex flex-col">
            <div class="flex items-center justify-between px-4 py-3 border-b border-vc-border">
                <h3 class="text-sm font-bold text-vc-text">${escapeHTML(channelName)} - Members</h3>
                <button onclick="document.getElementById('member-modal').remove()" class="text-vc-muted hover:text-vc-text transition">
                    <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/>
                    </svg>
                </button>
            </div>
            <div class="p-3 border-b border-vc-border">
                <div class="flex gap-2 mb-2">
                    <select id="member-user-select" class="flex-1 px-3 py-1.5 bg-vc-bg border border-vc-border rounded-lg text-sm text-vc-text focus:outline-none focus:border-vc-accent transition">
                        <option value="">Loading users...</option>
                    </select>
                    <button onclick="addMemberFromSelect(${channelId})" class="px-3 py-1.5 bg-vc-accent hover:bg-vc-accent/80 text-white text-sm font-medium rounded-lg transition">Add</button>
                </div>
                <button onclick="generateInviteLink(${channelId})" class="w-full px-3 py-1.5 bg-vc-channel hover:bg-vc-hover text-vc-text text-sm rounded-lg transition flex items-center justify-center gap-1.5">
                    <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13.828 10.172a4 4 0 00-5.656 0l-4 4a4 4 0 105.656 5.656l1.102-1.101m-.758-4.899a4 4 0 005.656 0l4-4a4 4 0 00-5.656-5.656l-1.1 1.1"/>
                    </svg>
                    Generate invite link
                </button>
                <div id="invite-link-result" class="hidden mt-2"></div>
            </div>
            <div id="member-list" class="flex-1 overflow-y-auto p-2 space-y-1">
                <div class="text-center text-vc-muted text-xs py-4">Loading...</div>
            </div>
        </div>
    `;
    modal.addEventListener('click', (e) => {
        if (e.target === modal) modal.remove();
    });
    document.body.appendChild(modal);
    loadMembers(channelId);
    loadUserSelect(channelId);
}

async function loadMembers(channelId) {
    try {
        const res = await fetch('/channels/members?id=' + channelId);
        if (res.status === 403) {
            document.getElementById('member-list').innerHTML = '<div class="text-center text-vc-red text-xs py-4">No permission to manage members</div>';
            return;
        }
        const data = await res.json();
        const list = document.getElementById('member-list');
        if (!data.members || data.members.length === 0) {
            list.innerHTML = '<div class="text-center text-vc-muted text-xs py-4">No members yet</div>';
            return;
        }
        list.innerHTML = data.members.map(m => `
            <div class="flex items-center justify-between px-2 py-1.5 rounded-lg hover:bg-vc-hover/30 transition">
                <div class="flex items-center gap-2">
                    <img src="${avatarURL(m.Username)}" alt="" class="w-6 h-6 rounded-full">
                    <span class="text-sm text-vc-text">${escapeHTML(m.Username)}</span>
                    ${m.UserID === data.created_by ? '<span class="text-[10px] text-vc-yellow bg-vc-yellow/10 px-1 rounded">owner</span>' : ''}
                </div>
                ${m.UserID !== data.created_by ? `
                    <button onclick="removeMember(${channelId}, ${m.UserID})" class="text-vc-muted hover:text-vc-red transition text-xs px-1.5 py-0.5 rounded hover:bg-vc-red/10">
                        Remove
                    </button>
                ` : ''}
            </div>
        `).join('');
    } catch (err) {
        console.error('Failed to load members:', err);
    }
}

async function loadUserSelect(channelId) {
    try {
        const [usersRes, membersRes] = await Promise.all([
            fetch('/api/users'),
            fetch('/channels/members?id=' + channelId),
        ]);
        const users = await usersRes.json();
        const membersData = await membersRes.json();
        const memberIds = new Set((membersData.members || []).map(m => m.UserID));

        const select = document.getElementById('member-user-select');
        if (!select) return;
        const available = users.filter(u => !memberIds.has(u.id));
        if (available.length === 0) {
            select.innerHTML = '<option value="">All users already added</option>';
        } else {
            select.innerHTML = '<option value="">Select user...</option>' +
                available.map(u => `<option value="${escapeHTML(u.username)}">${escapeHTML(u.username)}</option>`).join('');
        }
    } catch (err) {
        console.error('Failed to load users:', err);
    }
}

async function addMemberFromSelect(channelId) {
    const select = document.getElementById('member-user-select');
    const username = select ? select.value : '';
    if (!username) return;

    const form = new FormData();
    form.append('channel_id', channelId);
    form.append('username', username);
    form.append('csrf_token', getCSRFToken());

    try {
        const res = await fetch('/channels/members/add', { method: 'POST', body: form });
        if (res.status === 404) {
            alert('User not found');
            return;
        }
        if (!res.ok) {
            alert('Failed to add member');
            return;
        }
        loadMembers(channelId);
        loadUserSelect(channelId);
    } catch (err) {
        console.error('Failed to add member:', err);
    }
}

async function generateInviteLink(channelId) {
    const form = new FormData();
    form.append('channel_id', channelId);
    form.append('csrf_token', getCSRFToken());

    try {
        const res = await fetch('/channels/invite', { method: 'POST', body: form });
        if (!res.ok) {
            alert('Failed to generate invite link');
            return;
        }
        const data = await res.json();
        const url = window.location.origin + data.url;
        const container = document.getElementById('invite-link-result');
        if (!container) return;
        container.classList.remove('hidden');
        container.innerHTML = `
            <div class="flex gap-1.5">
                <input type="text" value="${escapeHTML(url)}" readonly
                    class="flex-1 px-2.5 py-1.5 bg-vc-bg border border-vc-border rounded-lg text-xs text-vc-text font-mono select-all focus:outline-none">
                <button onclick="navigator.clipboard.writeText('${url}').then(() => this.textContent = 'Copied!')"
                    class="px-2.5 py-1.5 bg-vc-accent hover:bg-vc-accent/80 text-white text-xs font-medium rounded-lg transition whitespace-nowrap">
                    Copy
                </button>
            </div>
            <div class="text-[10px] text-vc-muted mt-1">Link expires in 7 days</div>
        `;
    } catch (err) {
        console.error('Failed to generate invite:', err);
    }
}

async function removeMember(channelId, userId) {
    const form = new FormData();
    form.append('channel_id', channelId);
    form.append('user_id', userId);
    form.append('csrf_token', getCSRFToken());

    try {
        const res = await fetch('/channels/members/remove', { method: 'POST', body: form });
        if (!res.ok) {
            alert('Failed to remove member');
            return;
        }
        loadMembers(channelId);
        loadUserSelect(channelId);
    } catch (err) {
        console.error('Failed to remove member:', err);
    }
}

// --- Theme picker ---

const THEMES = [
    { id: 'default',    name: 'Default',    color: '#7c5cfc' },
    { id: 'midnight',   name: 'Midnight',   color: '#4f8ff7' },
    { id: 'forest',     name: 'Forest',     color: '#34d399' },
    { id: 'cherry',     name: 'Cherry',     color: '#f43f5e' },
    { id: 'amber',      name: 'Amber',      color: '#f59e0b' },
    { id: 'abyss',      name: 'Abyss',      color: '#a78bfa' },
    { id: 'light',      name: 'Light',      color: '#f5f5f7', border: '#d1d5db' },
    { id: 'light-warm', name: 'Warm Light', color: '#faf7f2', border: '#d6cfc5' },
    { id: 'light-sky',  name: 'Sky Light',  color: '#f0f7ff', border: '#bdd0e0' },
];

function setTheme(themeId) {
    document.documentElement.setAttribute('data-theme', themeId);
    localStorage.setItem('vocala-theme', themeId);
    // Update active indicator
    document.querySelectorAll('.theme-dot').forEach(el => {
        el.classList.toggle('ring-2', el.dataset.theme === themeId);
        el.classList.toggle('ring-white', el.dataset.theme === themeId);
        el.classList.toggle('ring-offset-2', el.dataset.theme === themeId);
        el.classList.toggle('ring-offset-vc-sidebar', el.dataset.theme === themeId);
    });
}

function toggleThemePicker() {
    const existing = document.getElementById('theme-picker');
    if (existing) { existing.remove(); return; }

    const current = localStorage.getItem('vocala-theme') || 'default';
    const picker = document.createElement('div');
    picker.id = 'theme-picker';
    picker.className = 'absolute bottom-14 left-2 bg-vc-channel border border-vc-border rounded-xl p-3 shadow-2xl z-50 fade-in';
    picker.innerHTML = `
        <div class="text-xs font-medium text-vc-muted mb-2">Theme</div>
        <div class="flex gap-2 flex-wrap max-w-[230px]">
            ${THEMES.map(t => `
                <button onclick="setTheme('${t.id}')" title="${t.name}"
                    class="theme-dot w-7 h-7 rounded-full transition-all hover:scale-110 ${t.id === current ? 'ring-2 ring-white ring-offset-2 ring-offset-vc-sidebar' : ''}"
                    data-theme="${t.id}"
                    style="background: ${t.color}${t.border ? '; box-shadow: inset 0 0 0 2px ' + t.border : ''}">
                </button>
            `).join('')}
        </div>
    `;

    // Close on outside click
    setTimeout(() => {
        document.addEventListener('click', function closePicker(e) {
            if (!picker.contains(e.target) && !e.target.closest('[title="Theme"]')) {
                picker.remove();
                document.removeEventListener('click', closePicker);
            }
        });
    }, 0);

    // Find the sidebar bottom panel to position relative to
    const bottomBar = document.querySelector('.border-t.border-vc-border .flex.items-center');
    if (bottomBar) {
        bottomBar.style.position = 'relative';
        bottomBar.appendChild(picker);
    } else {
        document.body.appendChild(picker);
    }
}

// --- Guest invite link ---

function createGuestLink(channelId) {
    const existing = document.getElementById('guest-link-popup');
    if (existing) { existing.remove(); return; }

    const popup = document.createElement('div');
    popup.id = 'guest-link-popup';
    popup.className = 'fixed top-20 right-4 bg-vc-sidebar border border-vc-border rounded-xl shadow-2xl p-4 z-50 w-80 fade-in';
    popup.innerHTML = `
        <div class="flex items-center justify-between mb-3">
            <span class="text-sm font-bold text-vc-text">Guest Invite Link</span>
            <button onclick="document.getElementById('guest-link-popup').remove()" class="text-vc-muted hover:text-vc-text">
                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/>
                </svg>
            </button>
        </div>
        <div class="flex gap-2 mb-3" id="guest-link-durations">
            <button data-hours="1" onclick="generateGuestLink(${channelId}, 1, this)" class="guest-dur flex-1 py-1.5 text-xs font-medium rounded-lg border border-vc-border hover:border-vc-accent hover:text-vc-accent transition text-vc-muted">1h</button>
            <button data-hours="6" onclick="generateGuestLink(${channelId}, 6, this)" class="guest-dur flex-1 py-1.5 text-xs font-medium rounded-lg border border-vc-border hover:border-vc-accent hover:text-vc-accent transition text-vc-muted">6h</button>
            <button data-hours="24" onclick="generateGuestLink(${channelId}, 24, this)" class="guest-dur flex-1 py-1.5 text-xs font-medium rounded-lg border border-vc-border hover:border-vc-accent hover:text-vc-accent transition text-vc-muted">24h</button>
            <button data-hours="72" onclick="generateGuestLink(${channelId}, 72, this)" class="guest-dur flex-1 py-1.5 text-xs font-medium rounded-lg border border-vc-border hover:border-vc-accent hover:text-vc-accent transition text-vc-muted">3d</button>
            <button data-hours="168" onclick="generateGuestLink(${channelId}, 168, this)" class="guest-dur flex-1 py-1.5 text-xs font-medium rounded-lg border border-vc-border hover:border-vc-accent hover:text-vc-accent transition text-vc-muted">7d</button>
        </div>
        <div id="guest-link-result" class="text-center text-xs text-vc-muted">Select link duration</div>
    `;
    document.body.appendChild(popup);
}

async function generateGuestLink(channelId, hours, btn) {
    const resultEl = document.getElementById('guest-link-result');
    if (!resultEl) return;

    // Update active state on duration buttons
    const inactiveCls = ['border-vc-border', 'hover:border-vc-accent', 'hover:text-vc-accent', 'text-vc-muted'];
    const activeCls = ['border-vc-accent', 'bg-vc-accent/10', 'text-vc-accent'];
    document.querySelectorAll('#guest-link-durations .guest-dur').forEach(b => {
        b.classList.remove(...activeCls);
        b.classList.add(...inactiveCls);
    });
    if (btn) {
        btn.classList.remove(...inactiveCls);
        btn.classList.add(...activeCls);
    }

    resultEl.innerHTML = '<span class="text-vc-muted">Generating...</span>';

    const form = new FormData();
    form.append('channel_id', channelId);
    form.append('hours', hours);
    form.append('csrf_token', getCSRFToken());

    try {
        const res = await fetch('/channels/guest-invite', { method: 'POST', body: form });
        if (!res.ok) {
            resultEl.innerHTML = '<span class="text-vc-red">Failed to generate link</span>';
            return;
        }
        const data = await res.json();
        const url = window.location.origin + data.url;
        const label = hours >= 24 ? Math.floor(hours / 24) + 'd' : hours + 'h';
        resultEl.innerHTML = `
            <div class="flex gap-1.5 mb-1.5">
                <input type="text" value="${escapeHTML(url)}" readonly
                    class="flex-1 px-2.5 py-1.5 bg-vc-bg border border-vc-border rounded-lg text-xs text-vc-text font-mono select-all focus:outline-none">
                <button onclick="navigator.clipboard.writeText('${url}').then(() => this.textContent = 'OK')"
                    class="px-2.5 py-1.5 bg-vc-accent hover:bg-vc-accent/80 text-white text-xs font-medium rounded-lg transition whitespace-nowrap">
                    Copy
                </button>
            </div>
            <div class="text-[10px] text-vc-muted">Expires in ${label}. Guest sees only this channel.</div>
        `;
    } catch (err) {
        resultEl.innerHTML = '<span class="text-vc-red">Request failed</span>';
    }
}

// --- Settings modal (devices, sounds) ---

let selectedMicId = localStorage.getItem('vocala-mic') || '';
let selectedCamId = localStorage.getItem('vocala-cam') || '';
let selectedSpkId = localStorage.getItem('vocala-spk') || '';

async function openSettings() {
    const old = document.getElementById('settings-modal');
    if (old) old.remove();

    const modal = document.createElement('div');
    modal.id = 'settings-modal';
    modal.className = 'fixed inset-0 z-50 flex items-center justify-center bg-black/60';
    modal.innerHTML = `
        <div class="bg-vc-sidebar border border-vc-border rounded-xl shadow-2xl w-[420px] max-h-[80vh] flex flex-col">
            <div class="flex items-center justify-between px-4 py-3 border-b border-vc-border">
                <h3 class="text-sm font-bold text-vc-text">Settings</h3>
                <button onclick="document.getElementById('settings-modal').remove()" class="text-vc-muted hover:text-vc-text transition">
                    <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/>
                    </svg>
                </button>
            </div>
            <div class="p-4 space-y-4 overflow-y-auto">
                <div>
                    <label class="block text-xs font-medium text-vc-muted mb-1">Microphone</label>
                    <select id="settings-mic" class="w-full px-3 py-2 bg-vc-bg border border-vc-border rounded-lg text-sm text-vc-text focus:outline-none focus:border-vc-accent transition">
                        <option value="">Loading...</option>
                    </select>
                </div>
                <div>
                    <label class="block text-xs font-medium text-vc-muted mb-1">Camera</label>
                    <select id="settings-cam" class="w-full px-3 py-2 bg-vc-bg border border-vc-border rounded-lg text-sm text-vc-text focus:outline-none focus:border-vc-accent transition">
                        <option value="">Loading...</option>
                    </select>
                </div>
                <div>
                    <label class="block text-xs font-medium text-vc-muted mb-1">Speaker</label>
                    <select id="settings-spk" class="w-full px-3 py-2 bg-vc-bg border border-vc-border rounded-lg text-sm text-vc-text focus:outline-none focus:border-vc-accent transition">
                        <option value="">Loading...</option>
                    </select>
                </div>
                <div class="border-t border-vc-border pt-3">
                    <label class="flex items-center justify-between cursor-pointer">
                        <span class="text-sm text-vc-text">Sound notifications</span>
                        <input type="checkbox" id="settings-sounds" ${notifSoundsEnabled ? 'checked' : ''}
                            class="rounded border-vc-border text-vc-accent focus:ring-vc-accent"
                            onchange="toggleSounds()">
                    </label>
                </div>
                <div class="border-t border-vc-border pt-3">
                    <div class="text-xs font-medium text-vc-muted mb-2">Change Password</div>
                    <div class="space-y-2">
                        <input type="password" id="settings-old-pw" placeholder="Current password"
                            class="w-full px-3 py-2 bg-vc-bg border border-vc-border rounded-lg text-sm text-vc-text placeholder-vc-muted focus:outline-none focus:border-vc-accent transition">
                        <input type="password" id="settings-new-pw" placeholder="New password (min 8 chars)"
                            class="w-full px-3 py-2 bg-vc-bg border border-vc-border rounded-lg text-sm text-vc-text placeholder-vc-muted focus:outline-none focus:border-vc-accent transition">
                        <button onclick="changePassword()" class="w-full px-3 py-2 bg-vc-channel hover:bg-vc-hover text-vc-text text-sm font-medium rounded-lg transition border border-vc-border">
                            Change Password
                        </button>
                        <div id="settings-pw-msg" class="hidden text-xs"></div>
                    </div>
                </div>
            </div>
            <div class="px-4 py-3 border-t border-vc-border flex justify-end">
                <button onclick="saveSettings()" class="px-4 py-2 bg-vc-accent hover:bg-vc-accent/80 text-white text-sm font-medium rounded-lg transition">
                    Save
                </button>
            </div>
        </div>
    `;
    modal.addEventListener('click', (e) => {
        if (e.target === modal) modal.remove();
    });
    document.body.appendChild(modal);
    await loadDeviceList();
}

async function loadDeviceList() {
    try {
        const devices = await navigator.mediaDevices.enumerateDevices();
        const micSelect = document.getElementById('settings-mic');
        const camSelect = document.getElementById('settings-cam');
        const spkSelect = document.getElementById('settings-spk');

        const mics = devices.filter(d => d.kind === 'audioinput');
        const cams = devices.filter(d => d.kind === 'videoinput');
        const spks = devices.filter(d => d.kind === 'audiooutput');

        micSelect.innerHTML = '<option value="">Default</option>' +
            mics.map(d => `<option value="${d.deviceId}" ${d.deviceId === selectedMicId ? 'selected' : ''}>${escapeHTML(d.label || 'Microphone ' + (mics.indexOf(d) + 1))}</option>`).join('');
        camSelect.innerHTML = '<option value="">Default</option>' +
            cams.map(d => `<option value="${d.deviceId}" ${d.deviceId === selectedCamId ? 'selected' : ''}>${escapeHTML(d.label || 'Camera ' + (cams.indexOf(d) + 1))}</option>`).join('');
        spkSelect.innerHTML = '<option value="">Default</option>' +
            spks.map(d => `<option value="${d.deviceId}" ${d.deviceId === selectedSpkId ? 'selected' : ''}>${escapeHTML(d.label || 'Speaker ' + (spks.indexOf(d) + 1))}</option>`).join('');
    } catch (e) {
        console.error('Failed to enumerate devices:', e);
    }
}

async function changePassword() {
    const oldPw = document.getElementById('settings-old-pw')?.value || '';
    const newPw = document.getElementById('settings-new-pw')?.value || '';
    const msgEl = document.getElementById('settings-pw-msg');

    if (!oldPw || !newPw) {
        msgEl.textContent = 'Fill in both fields';
        msgEl.className = 'text-xs text-vc-red';
        return;
    }
    if (newPw.length < 8) {
        msgEl.textContent = 'New password must be at least 8 characters';
        msgEl.className = 'text-xs text-vc-red';
        return;
    }

    const form = new FormData();
    form.append('old_password', oldPw);
    form.append('new_password', newPw);
    form.append('csrf_token', getCSRFToken());

    try {
        const res = await fetch('/account/password', { method: 'POST', body: form });
        const data = await res.json();
        if (res.ok) {
            msgEl.textContent = 'Password changed';
            msgEl.className = 'text-xs text-vc-green';
            document.getElementById('settings-old-pw').value = '';
            document.getElementById('settings-new-pw').value = '';
        } else {
            msgEl.textContent = data.error || 'Failed to change password';
            msgEl.className = 'text-xs text-vc-red';
        }
    } catch (e) {
        msgEl.textContent = 'Request failed';
        msgEl.className = 'text-xs text-vc-red';
    }
}

function saveSettings() {
    const mic = document.getElementById('settings-mic')?.value || '';
    const cam = document.getElementById('settings-cam')?.value || '';
    const spk = document.getElementById('settings-spk')?.value || '';

    selectedMicId = mic;
    selectedCamId = cam;
    selectedSpkId = spk;
    localStorage.setItem('vocala-mic', mic);
    localStorage.setItem('vocala-cam', cam);
    localStorage.setItem('vocala-spk', spk);

    // Apply speaker to all audio/video elements
    if (spk) {
        document.querySelectorAll('audio, video').forEach(el => {
            if (el.setSinkId) el.setSinkId(spk).catch(() => {});
        });
    }

    document.getElementById('settings-modal')?.remove();
}



function adjustUserVolume(username, value) {
    const vol = parseInt(value) / 100;
    setUserVolume(username, vol);
    // Apply volume to all remote audio elements
    // SFU assigns streamID "audio-{userID}" but we need username->userID mapping
    // For now, apply to all audio (works well with small groups)
    document.querySelectorAll('audio').forEach(el => {
        el.volume = vol;
    });
}

function getUserVolume(username) {
    if (userVolumes[username] !== undefined) return userVolumes[username];
    const saved = localStorage.getItem('vocala-vol-' + username);
    if (saved !== null) {
        userVolumes[username] = parseFloat(saved);
        return userVolumes[username];
    }
    return 1.0;
}
