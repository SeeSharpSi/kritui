function scrollMessages(root) {
    const messageList = root.matches?.('.message-list')
        ? root
        : root.querySelector?.('.message-list');
    if (messageList) {
        pinMessageList(messageList, true);
    }
}

const shellStates = new WeakMap();

function shellFor(root) {
    if (root?.matches?.('.shell-scroll')) {
        return root;
    }
    return root?.closest?.('.shell-scroll') ?? root?.querySelector?.('.shell-scroll') ?? null;
}

function shellHeaderSpace(shell) {
    const header = shell.querySelector(':scope > .app-header');
    if (!header) {
        return 0;
    }
    const style = getComputedStyle(header);
    return header.getBoundingClientRect().height
        + (parseFloat(style.marginTop) || 0)
        + (parseFloat(style.marginBottom) || 0);
}

const shellPaneSelector = '#message-list, #settings-page, #history-page';
const shellExcludedSelector = [
    'input',
    'textarea',
    'select',
    '[contenteditable]:not([contenteditable="false"])',
    '#message-form',
    '.command-autocomplete',
    '.chat-options',
    '.model-picker',
    '.tool-picker',
    '.append-picker',
].join(', ');
const shellTouchSlop = 4;

function shellActivePane(target) {
    const element = target instanceof Element ? target : target?.parentElement;
    const pane = element?.closest?.(shellPaneSelector);
    if (!pane || pane.hidden || !pane.isConnected) {
        return null;
    }
    const style = getComputedStyle(pane);
    if (style.display === 'none' || style.visibility === 'hidden') {
        return null;
    }
    return pane;
}

function shellPaneForInput(shell, target) {
    const element = target instanceof Element ? target : target?.parentElement;
    if (!element || element.closest(shellExcludedSelector)) {
        return null;
    }
    const pane = shellActivePane(target);
    if (!pane || !shell.contains(pane)) {
        return null;
    }
    let nested = element;
    while (nested && nested !== pane) {
        if (nested.scrollHeight > nested.clientHeight + 1) {
            const style = getComputedStyle(nested);
            if (style.overflowY === 'auto' || style.overflowY === 'scroll') {
                return null;
            }
        }
        nested = nested.parentElement;
    }
    return pane;
}

function shellHeaderVisible(shell, state) {
    return state.headerSpace > 0 && shell.scrollTop < state.headerSpace - 0.5;
}

function shellHeaderRemaining(shell, state) {
    const offset = Math.min(Math.max(shell.scrollTop, 0), state.headerSpace);
    return Math.max(0, state.headerSpace - offset);
}

function shellPayHeaderThenPane(shell, state, pane, delta) {
    const headerDelta = Math.min(delta, shellHeaderRemaining(shell, state));
    if (headerDelta > 0) {
        shell.scrollTop = Math.min(state.headerSpace, Math.max(shell.scrollTop, 0) + headerDelta);
        syncShellOffset(shell, state);
    }
    const residual = delta - headerDelta;
    if (residual <= 0) {
        return;
    }
    const maxScroll = Math.max(0, pane.scrollHeight - pane.clientHeight);
    if (maxScroll <= 0) {
        return;
    }
    pane.scrollTop = Math.min(maxScroll, Math.max(pane.scrollTop, 0) + residual);
}

function shellWheelPixels(event, pane) {
    if (event.deltaMode === 1) {
        return event.deltaY * 16;
    }
    if (event.deltaMode === 2) {
        return event.deltaY * Math.max(1, pane.clientHeight);
    }
    return event.deltaY;
}

function handleShellWheel(shell, state, event) {
    if (event.defaultPrevented || !event.cancelable || event.ctrlKey || event.shiftKey) {
        return;
    }
    if (event.deltaY <= 0 || !shellHeaderVisible(shell, state)) {
        return;
    }
    if (Math.abs(event.deltaX) > Math.abs(event.deltaY)) {
        return;
    }
    const pane = shellPaneForInput(shell, event.target);
    if (!pane) {
        return;
    }
    const delta = shellWheelPixels(event, pane);
    if (delta <= 0) {
        return;
    }
    event.preventDefault();
    shellPayHeaderThenPane(shell, state, pane, delta);
}

function handleShellTouchStart(shell, state, event) {
    state.touch = null;
    if (event.touches.length !== 1 || !shellHeaderVisible(shell, state)) {
        return;
    }
    const pane = shellPaneForInput(shell, event.target);
    if (!pane) {
        return;
    }
    const touch = event.touches[0];
    state.touch = {
        identifier: touch.identifier,
        startX: touch.clientX,
        startY: touch.clientY,
        lastY: touch.clientY,
        pane,
        claimed: false,
        reversing: false,
        revealBlocked: false,
    };
}

function shellTouchDelta(shell, state, gesture, delta) {
    const pane = gesture.pane;
    if (delta > 0) {
        gesture.reversing = false;
        shellPayHeaderThenPane(shell, state, pane, delta);
        return;
    }
    if (delta === 0) {
        return;
    }

    const upward = -delta;
    if (!gesture.reversing) {
        gesture.reversing = true;
        gesture.revealBlocked = pane.scrollTop > 0;
    }
    if (gesture.revealBlocked) {
        const applied = Math.min(Math.max(pane.scrollTop, 0), upward);
        if (applied > 0) {
            pane.scrollTop -= applied;
        }
        return;
    }

    const offset = Math.min(Math.max(shell.scrollTop, 0), state.headerSpace);
    const applied = Math.min(offset, upward);
    if (applied <= 0) {
        return;
    }
    shell.scrollTop = offset - applied;
    syncShellOffset(shell, state);
}

function handleShellTouchMove(shell, state, event) {
    const gesture = state.touch;
    if (!gesture) {
        return;
    }
    if (event.touches.length !== 1 || event.touches[0].identifier !== gesture.identifier) {
        state.touch = null;
        return;
    }
    const touch = event.touches[0];
    if (!event.cancelable || event.defaultPrevented) {
        gesture.lastY = touch.clientY;
        return;
    }
    if (!gesture.pane?.isConnected) {
        state.touch = null;
        return;
    }

    const horizontal = touch.clientX - gesture.startX;
    const downward = gesture.startY - touch.clientY;
    if (!gesture.claimed) {
        if (Math.max(Math.abs(horizontal), Math.abs(downward)) < shellTouchSlop) {
            return;
        }
        if (Math.abs(horizontal) >= Math.abs(downward) || downward <= 0) {
            state.touch = null;
            return;
        }
        gesture.claimed = true;
        gesture.lastY = gesture.startY;
    }

    event.preventDefault();
    const delta = gesture.lastY - touch.clientY;
    gesture.lastY = touch.clientY;
    if (delta !== 0) {
        shellTouchDelta(shell, state, gesture, delta);
    }
}

function handleShellTouchEnd(state, event) {
    const gesture = state.touch;
    if (!gesture) {
        return;
    }
    if (!Array.from(event.touches).some((touch) => touch.identifier === gesture.identifier)) {
        state.touch = null;
    }
}

function syncShellOffset(shell, state) {
    const maxScroll = Math.max(0, shell.scrollHeight - shell.clientHeight);
    const offset = Math.min(Math.max(shell.scrollTop, 0), state.headerSpace, maxScroll);
    shell.style.setProperty('--shell-scroll-offset', `${offset}px`);
}

function measureShell(shell, state) {
    state.headerSpace = Math.max(0, shellHeaderSpace(shell));
    shell.style.setProperty('--shell-header-space', `${state.headerSpace}px`);
    syncShellOffset(shell, state);
    return state.headerSpace;
}

function shellState(shell) {
    let state = shellStates.get(shell);
    if (state) {
        return state;
    }

    state = {
        headerSpace: 76,
        initialized: false,
        pendingInitial: false,
        initialAttempts: 0,
        touch: null,
        onScroll: () => syncShellOffset(shell, state),
        onWheel: (event) => handleShellWheel(shell, state, event),
        onTouchStart: (event) => handleShellTouchStart(shell, state, event),
        onTouchMove: (event) => handleShellTouchMove(shell, state, event),
        onTouchEnd: (event) => handleShellTouchEnd(state, event),
        onTouchCancel: () => {
            state.touch = null;
        },
        resizeObserver: null,
    };
    shell.addEventListener('scroll', state.onScroll, { passive: true });
    shell.addEventListener('wheel', state.onWheel, { passive: false });
    shell.addEventListener('touchstart', state.onTouchStart, { passive: true });
    shell.addEventListener('touchmove', state.onTouchMove, { passive: false });
    shell.addEventListener('touchend', state.onTouchEnd, { passive: true });
    shell.addEventListener('touchcancel', state.onTouchCancel, { passive: true });
    state.resizeObserver = new ResizeObserver(() => measureShell(shell, state));
    state.resizeObserver.observe(shell);
    const header = shell.querySelector(':scope > .app-header');
    if (header) {
        state.resizeObserver.observe(header);
    }
    shellStates.set(shell, state);
    return state;
}

function destroyShell(root) {
    const shell = root?.matches?.('.shell-scroll')
        ? root
        : root?.querySelector?.('.shell-scroll');
    const state = shell && shellStates.get(shell);
    if (!state) {
        return;
    }
    shell.removeEventListener('scroll', state.onScroll);
    shell.removeEventListener('wheel', state.onWheel);
    shell.removeEventListener('touchstart', state.onTouchStart);
    shell.removeEventListener('touchmove', state.onTouchMove);
    shell.removeEventListener('touchend', state.onTouchEnd);
    shell.removeEventListener('touchcancel', state.onTouchCancel);
    state.touch = null;
    state.resizeObserver.disconnect();
    shellStates.delete(shell);
}

function initializeShell(root) {
    const shell = shellFor(root);
    if (!shell?.isConnected) {
        return;
    }
    const state = shellState(shell);
    const headerSpace = measureShell(shell, state);
    if (state.initialized) {
        return;
    }

    const messageList = shell.querySelector('#message-list');
    const measurable = !messageList || messageList.clientHeight > 0;
    if (!measurable && state.initialAttempts < 2) {
        state.initialAttempts += 1;
        if (!state.pendingInitial) {
            state.pendingInitial = true;
            requestAnimationFrame(() => {
                state.pendingInitial = false;
                initializeShell(shell);
            });
        }
        return;
    }

    state.initialized = true;
    shell.scrollTop = 0;
    syncShellOffset(shell, state);
    if (messageList && measurable && messageList.scrollHeight - messageList.clientHeight > 2) {
        shell.scrollTop = headerSpace;
        syncShellOffset(shell, state);
    }
}

window.addEventListener('resize', () => {
    document.querySelectorAll('.shell-scroll').forEach((shell) => {
        const state = shellStates.get(shell);
        if (state) {
            measureShell(shell, state);
        }
    });
}, { passive: true });

const messageListStates = new WeakMap();
const messageBlockSelector = '.message, .completed-tool-calls, .chat-empty';

function refreshChatSummary() {
    document.body.dispatchEvent(new Event('chat-updated'));
}

function messageListFor(root) {
    return root?.matches?.('.message-list')
        ? root
        : root?.closest?.('.message-list') ?? root?.querySelector?.('.message-list');
}

function isMessageListAtBottom(messageList) {
    return messageList.scrollHeight - messageList.clientHeight - messageList.scrollTop <= 2;
}

function forEachMessageBlock(root, callback) {
    if (root.matches?.(messageBlockSelector)) {
        callback(root);
    }
    root.querySelectorAll?.(messageBlockSelector).forEach(callback);
}

function observeMessageBlocks(root, state) {
    forEachMessageBlock(root, (block) => {
        if (!state.observedBlocks.has(block)) {
            state.observedBlocks.add(block);
            state.resizeObserver.observe(block);
        }
    });
}

function unobserveMessageBlocks(root, state) {
    forEachMessageBlock(root, (block) => {
        if (state.observedBlocks.delete(block)) {
            state.resizeObserver.unobserve(block);
        }
    });
}

function messageListState(messageList) {
    let state = messageListStates.get(messageList);
    if (state) {
        return state;
    }

    state = {
        pinned: isMessageListAtBottom(messageList),
        restoreAfterSwap: false,
        forcing: false,
        frame: 0,
        viewportAnchor: null,
        observedBlocks: new WeakSet(),
    };
    state.resizeObserver = new ResizeObserver(() => pinMessageList(messageList));
    state.resizeObserver.observe(messageList);
    state.mutationObserver = new MutationObserver((mutations) => {
        for (const mutation of mutations) {
            mutation.removedNodes.forEach((node) => unobserveMessageBlocks(node, state));
            mutation.addedNodes.forEach((node) => observeMessageBlocks(node, state));
        }
        pinMessageList(messageList);
    });
    state.mutationObserver.observe(messageList, {
        childList: true,
        characterData: true,
        subtree: true,
    });
    messageList.addEventListener('scroll', () => {
        if (state.forcing || state.viewportAnchor) {
            return;
        }

        state.pinned = isMessageListAtBottom(messageList);
    });
    observeMessageBlocks(messageList, state);
    messageListStates.set(messageList, state);
    return state;
}

function destroyMessageListState(messageList) {
    const state = messageListStates.get(messageList);
    if (!state) {
        return;
    }

    state.resizeObserver.disconnect();
    state.mutationObserver.disconnect();
    if (state.frame) {
        cancelAnimationFrame(state.frame);
    }
    if (state.viewportAnchor?.raf) {
        cancelAnimationFrame(state.viewportAnchor.raf);
    }
    messageListStates.delete(messageList);
}

function destroyMessageListStates(root) {
    if (root?.matches?.('.message-list')) {
        destroyMessageListState(root);
    }
    root?.querySelectorAll?.('.message-list').forEach(destroyMessageListState);
}

function pinMessageList(messageList, force = false) {
    const state = messageListState(messageList);
    if (state.viewportAnchor) {
        return;
    }
    if (!force && !state.pinned) {
        return;
    }

    state.forcing ||= force;
    messageList.scrollTop = messageList.scrollHeight;
    if (state.frame) {
        return;
    }

    let previousHeight = -1;
    let stableFrames = 0;
    const pin = () => {
        state.frame = 0;
        if (state.viewportAnchor) {
            state.forcing = false;
            return;
        }
        if (!state.pinned && !state.forcing) {
            return;
        }

        messageList.scrollTop = messageList.scrollHeight;
        const height = messageList.scrollHeight;
        stableFrames = height === previousHeight && isMessageListAtBottom(messageList)
            ? stableFrames + 1
            : 0;
        previousHeight = height;

        if (stableFrames < 2) {
            state.frame = requestAnimationFrame(pin);
        } else {
            state.forcing = false;
            state.pinned = true;
        }
    };
    state.frame = requestAnimationFrame(pin);
}

function anchorMessageViewport(message, images, expanded) {
    const messageList = messageListFor(message);
    const bubble = message.querySelector('.message-bubble');
    if (!messageList || !bubble) {
        return;
    }

    const state = messageListState(messageList);
    if (state.viewportAnchor?.raf) {
        cancelAnimationFrame(state.viewportAnchor.raf);
    }
    state.viewportAnchor = null;
    if (state.frame) {
        cancelAnimationFrame(state.frame);
        state.frame = 0;
    }
    state.forcing = false;

    const anchor = state.viewportAnchor = {
        bubble,
        images,
        expanded,
        targetTop: bubble.getBoundingClientRect().top,
        raf: 0,
        priorHeight: getComputedStyle(images).height,
        spaceSettled: false,
        stableFrames: 0,
        frameCount: 0,
    };
    if (expanded) {
        messageList.style.setProperty('--message-viewport-anchor-space', `${messageList.clientHeight}px`);
    }
    message.classList.toggle('image-expanded', expanded);

    const adjust = () => {
        anchor.raf = 0;
        if (state.viewportAnchor !== anchor || !bubble.isConnected || !images.isConnected) {
            if (state.viewportAnchor === anchor) {
                messageList.style.removeProperty('--message-viewport-anchor-space');
                state.viewportAnchor = null;
            }
            return;
        }

        const currentTop = bubble.getBoundingClientRect().top;
        messageList.scrollTop += currentTop - anchor.targetTop;
        const height = getComputedStyle(images).height;
        const correctedTop = bubble.getBoundingClientRect().top;
        anchor.stableFrames = height === anchor.priorHeight && Math.abs(correctedTop - anchor.targetTop) < .5
            ? anchor.stableFrames + 1
            : 0;
        anchor.priorHeight = height;
        anchor.frameCount += 1;

        if (anchor.stableFrames >= 2 || anchor.frameCount >= 60) {
            if (!anchor.spaceSettled) {
                anchor.spaceSettled = true;
                if (anchor.expanded) {
                    const currentSpace = parseFloat(getComputedStyle(messageList).getPropertyValue('--message-viewport-anchor-space')) || 0;
                    const contentHeight = messageList.scrollHeight - currentSpace;
                    const neededSpace = Math.max(0, messageList.scrollTop + messageList.clientHeight - contentHeight + 1);
                    messageList.style.setProperty('--message-viewport-anchor-space', `${Math.min(currentSpace, neededSpace)}px`);
                } else {
                    messageList.style.removeProperty('--message-viewport-anchor-space');
                }
                anchor.stableFrames = 0;
                anchor.frameCount = 0;
                anchor.priorHeight = getComputedStyle(images).height;
                anchor.raf = requestAnimationFrame(adjust);
                return;
            }
            state.viewportAnchor = null;
            state.pinned = isMessageListAtBottom(messageList);
            return;
        }
        anchor.raf = requestAnimationFrame(adjust);
    };
    anchor.raf = requestAnimationFrame(adjust);
}

function rememberMessageScroll(root) {
    const messageList = messageListFor(root);
    if (messageList) {
        const state = messageListState(messageList);
        state.restoreAfterSwap = isMessageListAtBottom(messageList);
    }
}

function restoreMessageScroll(root) {
    const messageList = messageListFor(root);
    if (messageList && messageListState(messageList).restoreAfterSwap) {
        pinMessageList(messageList, true);
    }
}

const pendingExpandedToolCalls = new WeakMap();

function toolCallIDForElement(el) {
    return el.getAttribute?.('data-tool-call-id') || '';
}

function snapshotExpandedToolCalls(root) {
    const expanded = new Set();
    root.querySelectorAll?.('.tool-call-list .tool-call.expanded').forEach((call) => {
        const id = toolCallIDForElement(call);
        if (id) expanded.add(id);
    });
    return expanded;
}

function restoreExpandedToolCalls(root, expanded) {
    if (!expanded || expanded.size === 0) return;
    root.querySelectorAll?.('.tool-call-list .tool-call[data-tool-call-id]').forEach((call) => {
        const id = toolCallIDForElement(call);
        if (id && expanded.has(id)) {
            call.classList.add('expanded');
            call.querySelector('.tool-call-toggle')?.setAttribute('aria-expanded', 'true');
        }
    });
}

function syncPanelSendButton() {
    const sendButton = document.querySelector('#send-button');
    const attachButton = document.querySelector('#attach-button');
    const queueButton = document.querySelector('#queue-button');
    if (!sendButton) {
        return;
    }

    const panelOpen = document.querySelector('#page-panel > .panel-page:not([hidden])');
    const admissionPending = Boolean(document.querySelector('#message-form.htmx-request'));
    const completionActive = Boolean(document.querySelector('.loading-message.completion-active:not(.completion-failed)'));
    sendButton.disabled = Boolean(panelOpen || admissionPending);
    if (attachButton) {
        attachButton.disabled = Boolean(panelOpen || admissionPending);
    }
    if (queueButton) {
        queueButton.hidden = !completionActive;
        queueButton.disabled = Boolean(panelOpen || admissionPending);
    }

    const label = completionActive ? 'Steer' : 'Send';
    sendButton.setAttribute('aria-label', label);
    sendButton.title = completionActive ? 'Steer after the next model step' : 'Send message';
    const labelText = sendButton.querySelector('[data-send-label]');
    if (labelText) {
        labelText.textContent = label;
    }
}

function newSubmissionID() {
    if (typeof globalThis.crypto?.randomUUID === 'function') {
        return globalThis.crypto.randomUUID();
    }
    return `submission-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function ensureSubmissionID(form, body) {
    if (!form || !body) {
        return;
    }
    const input = form.querySelector('#message-submission');
    let submission = '';
    if (body instanceof FormData || body instanceof URLSearchParams) {
        submission = String(body.get('submission') || '');
    }
    if (!submission) {
        submission = input?.value || newSubmissionID();
    }
    if (input) {
        input.value = submission;
    }
    if (body instanceof FormData || body instanceof URLSearchParams) {
        body.set('submission', submission);
    }
}

function resetSubmissionID(form) {
    const input = form?.querySelector('#message-submission');
    if (input) {
        input.value = '';
    }
}

function submitMessageForm(form) {
    const submitter = form?.querySelector('#send-button');
    form?.requestSubmit(submitter ?? undefined);
}

function settingsClearState(button, pending) {
    const section = button.closest('.settings-section');
    const scope = section || button.closest('.settings-main-form');
    const apiKey = scope?.querySelector('input[type="password"]');
    const clearKey = scope?.querySelector('input[type="checkbox"][name^="clear_"]');
    const openButton = scope?.querySelector('[data-settings-clear-open]');
    const pendingMessage = scope?.querySelector('[data-settings-clear-pending]');
    if (!scope || !apiKey || !clearKey || !openButton || !pendingMessage) {
        return;
    }

    clearKey.checked = pending;
    apiKey.disabled = pending;
    if (pending) {
        apiKey.value = '';
    }
    openButton.hidden = pending;
    pendingMessage.hidden = !pending;
}

function resetPromptField(button, selector, datasetKey) {
    const field = button.closest('.settings-main-form')?.querySelector(selector);
    const defaultValue = field?.dataset[datasetKey];
    if (!field || defaultValue === undefined) {
        return;
    }
    field.value = defaultValue;
    field.dispatchEvent(new Event('input', { bubbles: true }));
    field.dispatchEvent(new Event('change', { bubbles: true }));
    field.focus({ preventScroll: true });
}

function resetSystemPrompt(button) {
    resetPromptField(button, '#system-prompt', 'defaultSystemPrompt');
}

function resetCompactionPrompt(button) {
    resetPromptField(button, '#compaction-prompt', 'defaultCompactionPrompt');
}

function syncToolResultElisionControls(root = document) {
    const forms = [];
    if (root?.matches?.('.settings-main-form')) {
        forms.push(root);
    }
    root?.querySelectorAll?.('.settings-main-form').forEach((form) => forms.push(form));

    forms.forEach((form) => {
        const mode = form.querySelector('#tool-result-elision-mode')?.value || 'none';
        form.querySelectorAll('[data-tool-elision-mode]').forEach((row) => {
            const active = row.dataset.toolElisionMode === mode;
            row.hidden = !active;
            row.querySelectorAll('input, select, textarea').forEach((control) => {
                control.disabled = !active;
                control.required = active;
            });
        });
    });
}

const attachmentStates = new WeakMap();
const imageTypes = new Set(['image/png', 'image/jpeg', 'image/jpg', 'image/webp']);
const maxAttachmentCount = 4;
const maxAttachmentBytes = 20 * 1024 * 1024;
const maxAttachmentTotalBytes = 80 * 1024 * 1024;

function attachmentIsPDF(file) {
    const type = (file?.type || '').toLowerCase();
    if (type === 'application/pdf') {
        return true;
    }
    if (type === '' || type === 'application/octet-stream') {
        return (file?.name || '').toLowerCase().endsWith('.pdf');
    }
    return false;
}

function acceptableAttachmentFile(file) {
    const type = (file?.type || '').toLowerCase();
    if (imageTypes.has(type) || attachmentIsPDF(file)) {
        return true;
    }
    if (type === '' || type === 'application/octet-stream') {
        const name = (file?.name || '').toLowerCase();
        return name.endsWith('.jpg') || name.endsWith('.jpeg');
    }
    return false;
}

function attachmentState(input) {
    let state = attachmentStates.get(input);
    if (!state) {
        state = { files: [], urls: [] };
        attachmentStates.set(input, state);
    }
    return state;
}

function syncComposerMessageRequirement(input) {
    const messageInput = input.closest('form')?.querySelector('#message');
    if (messageInput) {
        messageInput.required = attachmentState(input).files.length === 0;
    }
}

const imageViewer = {
    trigger: null,
    restoreFocus: true,
};

function imageViewerDialog() {
    return document.getElementById('image-viewer');
}

function imageViewerImage(dialog = imageViewerDialog()) {
    return dialog?.querySelector('#image-viewer-image') ?? null;
}

function imageViewerTriggerFor(target) {
    return target instanceof Element ? target.closest('img[data-image-viewer]') : null;
}

function openImageViewer(trigger) {
    if (!(trigger instanceof HTMLImageElement)) {
        return;
    }
    const dialog = imageViewerDialog();
    const image = imageViewerImage(dialog);
    const src = trigger.currentSrc || trigger.src;
    if (!dialog || !image || !src) {
        return;
    }

    const wasOpen = dialog.open;
    imageViewer.trigger = trigger;
    imageViewer.restoreFocus = true;
    image.src = src;
    image.alt = trigger.title || trigger.alt || 'Attached image';
    if (!wasOpen) {
        dialog.showModal();
    }
}

function clearImageViewer() {
    const image = imageViewerImage();
    if (image) {
        image.removeAttribute('src');
        image.alt = '';
    }
}

function closeImageViewer(restoreFocus = true) {
    const dialog = imageViewerDialog();
    if (!dialog?.open) {
        return;
    }
    imageViewer.restoreFocus = restoreFocus;
    dialog.close();
}

function closeImageViewerWithin(root, restoreFocus = true) {
    const trigger = imageViewer.trigger;
    if (!trigger || !root) {
        return;
    }
    if (root === trigger || root.contains?.(trigger)) {
        closeImageViewer(restoreFocus);
    }
}

function handleImageViewerClose() {
    if (imageViewerDialog()?.open) {
        return;
    }

    const trigger = imageViewer.trigger;
    const restoreFocus = imageViewer.restoreFocus;
    imageViewer.trigger = null;
    imageViewer.restoreFocus = true;
    clearImageViewer();
    if (restoreFocus && trigger?.isConnected) {
        trigger.focus({ preventScroll: true });
    }
}

imageViewerDialog()?.addEventListener('close', handleImageViewerClose);

const pdfViewer = {
    trigger: null,
    restoreFocus: true,
};

function pdfViewerDialog() {
    return document.getElementById('pdf-viewer');
}

function pdfViewerFrame(dialog = pdfViewerDialog()) {
    return dialog?.querySelector('#pdf-viewer-frame') ?? null;
}

function pdfViewerOpenLink(dialog = pdfViewerDialog()) {
    return dialog?.querySelector('#pdf-viewer-open') ?? null;
}

function pdfViewerName(dialog = pdfViewerDialog()) {
    return dialog?.querySelector('#pdf-viewer-name') ?? null;
}

const pdfCitationPathPattern = /^\/chats\/[1-9]\d*\/messages\/[1-9]\d*\/attachments\/\d+$/;

function pdfCitationURL(anchor) {
    if (!(anchor instanceof HTMLAnchorElement) || !anchor.closest('.message-content.markdown')) {
        return null;
    }
    let url;
    try {
        url = new URL(anchor.getAttribute('href') ?? '', window.location.href);
    } catch {
        return null;
    }
    if (url.origin !== window.location.origin || !pdfCitationPathPattern.test(url.pathname)) {
        return null;
    }
    return url;
}

function isPlainPrimaryClick(event) {
    return event.button === 0 && !event.ctrlKey && !event.metaKey && !event.shiftKey && !event.altKey;
}

function pdfViewerTriggerFor(target) {
    if (!(target instanceof Element)) {
        return null;
    }
    const button = target.closest('button[data-pdf-viewer]');
    if (button) {
        return button;
    }
    const anchor = target.closest('.message-content.markdown a[href]');
    return anchor && pdfCitationURL(anchor) ? anchor : null;
}

function openPdfViewer(trigger) {
    const dialog = pdfViewerDialog();
    const frame = pdfViewerFrame(dialog);
    const link = pdfViewerOpenLink(dialog);
    const nameElement = pdfViewerName(dialog);
    if (!dialog || !frame || !link) {
        return;
    }

    let url;
    let name;
    if (trigger instanceof HTMLButtonElement) {
        url = trigger.dataset.pdfUrl;
        name = trigger.dataset.pdfName || trigger.title || 'Attached PDF';
    } else if (trigger instanceof HTMLAnchorElement) {
        const citationURL = pdfCitationURL(trigger);
        if (!citationURL) {
            return;
        }
        url = citationURL.href;
        name = trigger.dataset.pdfName || trigger.title || trigger.textContent.trim() || 'Attached PDF';
    } else {
        return;
    }
    if (!url) {
        return;
    }

    const wasOpen = dialog.open;
    pdfViewer.trigger = trigger;
    pdfViewer.restoreFocus = true;
    frame.src = url;
    frame.title = `PDF document: ${name}`;
    link.href = url;
    if (nameElement) {
        nameElement.textContent = name;
    }
    dialog.setAttribute('aria-label', `PDF viewer: ${name}`);
    if (!wasOpen) {
        dialog.showModal();
    }
}

function clearPdfViewer() {
    const dialog = pdfViewerDialog();
    const frame = pdfViewerFrame(dialog);
    const link = pdfViewerOpenLink(dialog);
    const nameElement = pdfViewerName(dialog);
    if (frame) {
        frame.removeAttribute('src');
        frame.title = 'PDF document';
    }
    if (link) {
        link.removeAttribute('href');
    }
    if (nameElement) {
        nameElement.textContent = '';
    }
    dialog?.setAttribute('aria-label', 'PDF viewer');
}

function closePdfViewer(restoreFocus = true) {
    const dialog = pdfViewerDialog();
    if (!dialog?.open) {
        return;
    }
    pdfViewer.restoreFocus = restoreFocus;
    dialog.close();
}

function closePdfViewerWithin(root, restoreFocus = true) {
    const trigger = pdfViewer.trigger;
    if (!trigger || !root) {
        return;
    }
    if (root === trigger || root.contains?.(trigger)) {
        closePdfViewer(restoreFocus);
    }
}

function handlePdfViewerClose() {
    if (pdfViewerDialog()?.open) {
        return;
    }

    const trigger = pdfViewer.trigger;
    const restoreFocus = pdfViewer.restoreFocus;
    pdfViewer.trigger = null;
    pdfViewer.restoreFocus = true;
    clearPdfViewer();
    if (restoreFocus && trigger?.isConnected) {
        trigger.focus({ preventScroll: true });
    }
}

pdfViewerDialog()?.addEventListener('close', handlePdfViewerClose);

function renderAttachmentPreviews(input) {
    const main = input.closest('main');
    const previews = main?.querySelector('#image-previews');
    closeImageViewerWithin(previews, false);
    const state = attachmentState(input);
    state.urls.forEach((url) => URL.revokeObjectURL(url));
    state.urls = [];
    if (previews) {
        previews.replaceChildren();
        previews.hidden = state.files.length === 0;
    }

    state.files.forEach((file, index) => {
        if (!previews) {
            return;
        }
        const pdf = attachmentIsPDF(file);
        const name = file.name || (pdf ? `PDF ${index + 1}` : `Image ${index + 1}`);
        const wrapper = document.createElement('div');
        wrapper.className = 'image-preview attachment-preview';
        const remove = document.createElement('button');
        remove.className = 'image-preview-remove';
        remove.type = 'button';
        remove.dataset.imageIndex = String(index);
        remove.setAttribute('aria-label', `Remove ${name}`);
        remove.title = 'Remove attachment';
        remove.textContent = '\u00d7';
        if (pdf) {
            wrapper.classList.add('attachment-pdf');
            const label = document.createElement('span');
            label.className = 'attachment-pdf-label';
            label.textContent = 'PDF';
            const filename = document.createElement('span');
            filename.className = 'attachment-pdf-name';
            filename.textContent = name;
            filename.title = name;
            wrapper.append(label, filename, remove);
        } else {
            wrapper.classList.add('attachment-image');
            const image = document.createElement('img');
            image.alt = '';
            image.title = name;
            image.dataset.imageViewer = '';
            image.setAttribute('role', 'button');
            image.tabIndex = 0;
            image.setAttribute('aria-haspopup', 'dialog');
            image.setAttribute('aria-controls', 'image-viewer');
            image.setAttribute('aria-label', `Open ${name}`);
            const url = URL.createObjectURL(file);
            state.urls.push(url);
            image.src = url;
            wrapper.append(image, remove);
        }
        previews.append(wrapper);
    });
    if (previews) {
        previews.scrollLeft = previews.scrollWidth;
    }
    syncComposerMessageRequirement(input);
}

function announceAttachments(input, message) {
    const status = input.closest('form')?.querySelector('#image-paste-status');
    if (status) {
        status.textContent = message;
    }
}

function clearAttachments(input) {
    const state = attachmentState(input);
    state.files = [];
    input.value = '';
    renderAttachmentPreviews(input);
    announceAttachments(input, '');
}

function addAttachments(input, files) {
    const state = attachmentState(input);
    const accepted = [];
    const rejected = [];
    let totalBytes = state.files.reduce((total, file) => total + file.size, 0);
    files.forEach((file) => {
        const name = file.name || 'Attachment';
        if (!acceptableAttachmentFile(file)) {
            rejected.push(`${name}: unsupported type`);
        } else if (file.size > maxAttachmentBytes) {
            rejected.push(`${name}: too large`);
        } else if (state.files.length + accepted.length >= maxAttachmentCount) {
            rejected.push(`${name}: attachment limit reached`);
        } else if (totalBytes + file.size > maxAttachmentTotalBytes) {
            rejected.push(`${name}: total size limit reached`);
        } else {
            accepted.push(file);
            totalBytes += file.size;
        }
    });
    state.files.push(...accepted);
    renderAttachmentPreviews(input);
    const result = `${accepted.length} attachment${accepted.length === 1 ? '' : 's'} added.`
        + (rejected.length ? ` ${rejected.join('; ')}.` : '');
    announceAttachments(input, result);
}

function clearComposerAttachments(root) {
    const input = root.matches?.('#image-input')
        ? root
        : root.querySelector?.('#image-input');
    if (input) {
        const state = attachmentStates.get(input);
        const previews = input.closest('main')?.querySelector('#image-previews');
        const viewerSource = imageViewerImage()?.getAttribute('src') ?? '';
        if (state?.urls.includes(viewerSource)) {
            closeImageViewer(false);
        } else {
            closeImageViewerWithin(previews, false);
        }
        if (state) {
            state.urls.forEach((url) => URL.revokeObjectURL(url));
            state.urls = [];
            state.files = [];
        }
        attachmentStates.delete(input);
    }
}

function commandAutocompleteFor(input) {
    const autocompleteID = input?.getAttribute('aria-controls');
    return autocompleteID ? document.getElementById(autocompleteID) : null;
}

function visibleCommandOptions(autocomplete) {
    return Array.from(autocomplete.querySelectorAll('.command-option:not([hidden])'));
}

function closeCommandAutocomplete(input = document.querySelector('#message')) {
    const autocomplete = commandAutocompleteFor(input);
    if (!autocomplete) {
        return;
    }

    autocomplete.hidden = true;
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
    autocomplete.querySelectorAll('.command-option').forEach((option) => {
        option.setAttribute('aria-selected', 'false');
    });
}

function selectCommandOption(input, option) {
    const autocomplete = commandAutocompleteFor(input);
    if (!autocomplete || !option) {
        return;
    }

    autocomplete.querySelectorAll('.command-option').forEach((candidate) => {
        candidate.setAttribute('aria-selected', String(candidate === option));
    });
    input.setAttribute('aria-activedescendant', option.id);
    option.scrollIntoView({ block: 'nearest' });
}

function updateCommandAutocomplete(input) {
    const autocomplete = commandAutocompleteFor(input);
    if (!autocomplete || !/^\/[a-z0-9_-]*$/.test(input.value)) {
        closeCommandAutocomplete(input);
        return;
    }

    const query = input.value.slice(1);
    let firstMatch = null;
    autocomplete.querySelectorAll('.command-option').forEach((option) => {
        const matches = option.dataset.commandName.startsWith(query);
        option.hidden = !matches;
        option.setAttribute('aria-selected', 'false');
        firstMatch ||= matches ? option : null;
    });
    if (!firstMatch) {
        closeCommandAutocomplete(input);
        return;
    }

    autocomplete.hidden = false;
    input.setAttribute('aria-expanded', 'true');
    selectCommandOption(input, firstMatch);
}

function moveCommandSelection(input, offset) {
    const autocomplete = commandAutocompleteFor(input);
    if (!autocomplete || autocomplete.hidden) {
        return false;
    }

    const options = visibleCommandOptions(autocomplete);
    if (options.length === 0) {
        return false;
    }
    const selectedIndex = options.findIndex((option) => option.getAttribute('aria-selected') === 'true');
    const nextIndex = (selectedIndex + offset + options.length) % options.length;
    selectCommandOption(input, options[nextIndex]);
    return true;
}

function autoresizeMessageInput(el) {
    if (!el) {
        return;
    }
    el.style.height = 'auto';
    el.style.height = el.scrollHeight + 'px';
}

function autoresizeAllMessageInputs(root = document) {
    if (root?.matches?.('#message')) {
        autoresizeMessageInput(root);
    }
    root?.querySelectorAll?.('#message').forEach(autoresizeMessageInput);
}

function isCoarsePointer() {
    return window.matchMedia?.('(pointer: coarse)').matches === true;
}

function activateCommandOption(input, option) {
    input.value = `/${option.dataset.commandName}`;
    closeCommandAutocomplete(input);
    autoresizeMessageInput(input);
    if (option.dataset.commandRequiresArguments === 'true') {
        input.value += ' ';
        input.focus();
        input.setSelectionRange(input.value.length, input.value.length);
        return;
    }

    submitMessageForm(input.form);
}

function closeModelOptions() {
    const chatOptions = document.querySelector('.chat-options[open]');
    if (!chatOptions) {
        return;
    }
    chatOptions.removeAttribute('open');
    chatOptions.querySelectorAll('.model-picker[open], .tool-picker[open], .append-picker[open]').forEach((picker) => {
        picker.removeAttribute('open');
    });
}

function togglePanel(panelID, panelButton) {
    const selectedPanel = document.getElementById(panelID);
    if (!selectedPanel) {
        return;
    }

    const opening = selectedPanel.hidden;
    if (opening) {
        closeModelOptions();
    }
    document.querySelectorAll('#page-panel > .panel-page').forEach((panel) => {
        panel.hidden = !opening || panel !== selectedPanel;
    });
    document.querySelectorAll('[data-panel-target]').forEach((button) => {
        const active = opening && button.dataset.panelTarget === panelID;
        button.classList.toggle('active', active);
        button.setAttribute('aria-expanded', String(active));
    });
    if (opening && panelID === 'history-page') {
        selectedPanel.dispatchEvent(new Event('history-open'));
    }
    if (opening) {
        const focusTarget = selectedPanel.matches('[data-panel-initial-focus]')
            ? selectedPanel
            : selectedPanel.querySelector('[data-panel-initial-focus]');
        focusTarget?.focus();
    } else {
        panelButton?.focus();
    }
    syncPanelSendButton();
}

function showCompletionNetworkError(event, message) {
    const source = event.detail?.ctx?.sourceElement ?? event.target;
    const pending = source?.closest?.('.loading-message') ?? source;
    if (!pending?.matches?.('.loading-message') || pending.classList.contains('completion-failed')) {
        return;
    }

    const progress = pending.querySelector('.completion-progress');
    const failure = pending.querySelector('.completion-network-error');
    if (!failure) {
        return;
    }

    const errorMessage = failure.querySelector('[data-completion-error-message]');
    if (errorMessage) {
        errorMessage.textContent = message;
    }
    if (progress) {
        progress.hidden = true;
    }
    failure.hidden = false;
    pending.classList.add('completion-failed');
    syncPanelSendButton();
}

document.addEventListener('DOMContentLoaded', () => {
    scrollMessages(document);
    initializeShell(document);
    syncPanelSendButton();
    syncToolResultElisionControls(document);
    autoresizeAllMessageInputs(document);
    if ('serviceWorker' in navigator) {
        navigator.serviceWorker.register('/sw.js');
    }
});

function handleImagePointer(event, expanded) {
    if (event.pointerType === 'touch') {
        return;
    }

    const image = event.target.closest?.('.message.user .message-image');
    if (!image) {
        return;
    }
    const message = image.closest('.message.user');
    const relatedImage = event.relatedTarget instanceof Node
        ? event.relatedTarget.closest?.('.message.user .message-image')
        : null;
    if (relatedImage && relatedImage.closest('.message.user') === message) {
        return;
    }
    const images = image.closest('.message-images');
    if (message && images) {
        anchorMessageViewport(message, images, expanded);
    }
}

document.addEventListener('pointerover', (event) => handleImagePointer(event, true));
document.addEventListener('pointerout', (event) => handleImagePointer(event, false));

document.addEventListener('input', (event) => {
    if (event.target.matches('#message')) {
        autoresizeMessageInput(event.target);
        updateCommandAutocomplete(event.target);
    }
});

document.addEventListener('paste', (event) => {
    const input = event.target;
    if (!input.matches?.('#message')) {
        return;
    }
    const items = Array.from(event.clipboardData?.items ?? [])
        .filter((item) => item.kind === 'file')
        .map((item) => item.getAsFile())
        .filter((file) => file && acceptableAttachmentFile(file));
    const files = items.length > 0
        ? items
        : Array.from(event.clipboardData?.files ?? []).filter((file) => acceptableAttachmentFile(file));
    if (files.length === 0) {
        return;
    }
    event.preventDefault();
    const imageInput = input.form?.querySelector('#image-input');
    if (imageInput) {
        addAttachments(imageInput, files);
    }
});

document.addEventListener('change', (event) => {
    const input = event.target;
    if (input.matches?.('#tool-result-elision-mode')) {
        syncToolResultElisionControls(input.closest('form'));
        return;
    }
    if (!input.matches?.('#image-input')) {
        return;
    }
    const selected = Array.from(input.files || []);
    if (selected.length > 0) {
        addAttachments(input, selected);
    }
});

document.addEventListener('reset', (event) => {
    if (event.target.matches?.('.settings-main-form')) {
        queueMicrotask(() => syncToolResultElisionControls(event.target));
    }
});

document.addEventListener('focusin', (event) => {
    if (event.target.matches('#message')) {
        updateCommandAutocomplete(event.target);
    }
});

document.addEventListener('keydown', (event) => {
    const input = event.target;

    if (!event.isComposing && (event.key === 'Enter' || event.key === ' ')) {
        const viewerTrigger = imageViewerTriggerFor(input);
        if (viewerTrigger) {
            event.preventDefault();
            if (!event.repeat) {
                openImageViewer(viewerTrigger);
            }
            return;
        }
    }

    if (input.matches('.message-edit-input')) {
        if (event.key === 'Escape' && !event.isComposing) {
            event.preventDefault();
            closeMessageEditor(input.closest('.message.user'));
        }
        return;
    }
    if (!input.matches('#message') || event.isComposing) {
        return;
    }

    if (event.ctrlKey && !event.metaKey && !event.altKey && (event.key === 'j' || event.key === 'J')) {
        event.preventDefault();
        const start = input.selectionStart ?? input.value.length;
        const end = input.selectionEnd ?? input.value.length;
        input.value = input.value.slice(0, start) + '\n' + input.value.slice(end);
        const caret = start + 1;
        input.setSelectionRange(caret, caret);
        autoresizeMessageInput(input);
        updateCommandAutocomplete(input);
        return;
    }

    switch (event.key) {
    case 'ArrowDown':
        if (moveCommandSelection(input, 1)) {
            event.preventDefault();
        }
        break;
    case 'ArrowUp':
        if (moveCommandSelection(input, -1)) {
            event.preventDefault();
        }
        break;
    case 'Enter': {
        if (event.shiftKey) {
            break;
        }
        const autocomplete = commandAutocompleteFor(input);
        const selected = autocomplete?.hidden
            ? null
            : autocomplete?.querySelector('.command-option[aria-selected="true"]:not([hidden])');
        if (selected) {
            event.preventDefault();
            activateCommandOption(input, selected);
        } else if (isCoarsePointer()) {
            break;
        } else {
            event.preventDefault();
            submitMessageForm(input.form);
        }
        break;
    }
    case 'Escape':
        if (commandAutocompleteFor(input)?.hidden === false) {
            event.preventDefault();
            closeCommandAutocomplete(input);
        }
        break;
    case 'Tab':
        closeCommandAutocomplete(input);
        break;
    }
});

function closeMessageEditor(message, restoreFocus = true) {
    if (!message) {
        return;
    }
    message.querySelector('.message-edit-form')?.reset();
    const error = message.querySelector('.message-edit-error');
    if (error) {
        error.textContent = '';
        error.hidden = true;
    }
    message.classList.remove('editing');
    const button = message.querySelector('.message-edit-toggle');
    button?.setAttribute('aria-expanded', 'false');
    if (restoreFocus) {
        button?.focus();
    }
}

async function writeClipboardText(text) {
    if (navigator.clipboard?.writeText) {
        try {
            await navigator.clipboard.writeText(text);
            return true;
        } catch {
            // Fall back for browsers that expose Clipboard API but deny access.
        }
    }

    const activeElement = document.activeElement;
    const textarea = document.createElement('textarea');
    textarea.value = text;
    textarea.readOnly = true;
    textarea.style.position = 'fixed';
    textarea.style.opacity = '0';
    textarea.style.pointerEvents = 'none';
    document.body.append(textarea);
    textarea.select();
    textarea.setSelectionRange(0, textarea.value.length);
    let copied = false;
    try {
        copied = document.execCommand('copy');
    } catch {
        copied = false;
    }
    textarea.remove();
    if (activeElement instanceof HTMLElement) {
        activeElement.focus({ preventScroll: true });
    }
    return copied;
}

const messageCopyResetTimers = new WeakMap();
const shareCopyBusy = new WeakSet();
const shareStatusResetTimers = new WeakMap();

function shareActionsFor(element) {
    return element?.closest?.('.message-share-actions') ?? null;
}

function clearShareStatusTimer(actions) {
    const timer = shareStatusResetTimers.get(actions);
    if (timer === undefined) {
        return;
    }
    clearTimeout(timer);
    shareStatusResetTimers.delete(actions);
}

function setShareStatus(actions, message, state = '') {
    const status = actions?.querySelector('.message-share-status');
    if (!status) {
        return;
    }
    clearShareStatusTimer(actions);
    status.textContent = message;
    status.classList.toggle('is-copied', state === 'copied');
    status.classList.toggle('is-error', state === 'error');
    if (message && state === 'copied') {
        shareStatusResetTimers.set(actions, setTimeout(() => {
            setShareStatus(actions, '');
        }, 4000));
    }
}

async function copyShareLink(button) {
    const shareURL = button?.dataset?.shareUrl;
    const actions = shareActionsFor(button);
    if (!shareURL || !actions || shareCopyBusy.has(button)) {
        return false;
    }

    let url;
    try {
        url = new URL(shareURL, window.location.origin).href;
    } catch {
        setShareStatus(actions, 'Copy failed. Try again.', 'error');
        return false;
    }

    shareCopyBusy.add(button);
    setShareStatus(actions, 'Copying link...');
    const copied = await writeClipboardText(url);
    shareCopyBusy.delete(button);
    setShareStatus(
        actions,
        copied ? 'Link copied' : 'Copy failed. Try again.',
        copied ? 'copied' : 'error',
    );
    return copied;
}

function autoCopyShareLinks(root) {
    if (!root) {
        return;
    }
    const buttons = [];
    if (root.matches?.('.message-share-copy-button[data-share-copy-pending]')) {
        buttons.push(root);
    }
    root.querySelectorAll?.('.message-share-copy-button[data-share-copy-pending]').forEach((button) => {
        buttons.push(button);
    });
    buttons.forEach((button) => {
        button.removeAttribute('data-share-copy-pending');
        copyShareLink(button);
    });
}

async function copyMessage(button) {
    const text = button.closest('.message.user')?.querySelector('.message-content')?.textContent;
    if (text === undefined) {
        return;
    }

    const copied = await writeClipboardText(text);

    const label = copied ? 'Copied' : 'Copy failed';
    button.classList.remove('copied', 'copy-failed');
    button.classList.add(copied ? 'copied' : 'copy-failed');
    button.setAttribute('aria-label', label);
    button.title = label;

    clearTimeout(messageCopyResetTimers.get(button));
    messageCopyResetTimers.set(button, setTimeout(() => {
        button.classList.remove('copied', 'copy-failed');
        button.setAttribute('aria-label', 'Copy message');
        button.title = 'Copy message';
        messageCopyResetTimers.delete(button);
    }, 1600));
}

document.addEventListener('htmx:after:process', (event) => {
    scrollMessages(event.target);
    initializeShell(event.target);
    syncToolResultElisionControls(event.target);
});
document.addEventListener('htmx:config:request', (event) => {
    const source = event.detail?.ctx?.sourceElement;
    const form = source?.matches?.('#message-form')
        ? source
        : source?.closest?.('#message-form');
    if (!form) {
        return;
    }
    const body = event.detail?.ctx?.request?.body;
    ensureSubmissionID(form, body);
    const submitter = event.detail?.ctx?.request?.submitter;
    if (submitter?.name && (body instanceof FormData || body instanceof URLSearchParams) && !body.has(submitter.name)) {
        body.append(submitter.name, submitter.value);
    }
    const imageInput = form.querySelector('#image-input');
    if (!imageInput || !(body instanceof FormData)) {
        return;
    }
    body.delete('image');
    attachmentState(imageInput).files.forEach((file) => {
        body.append('image', file, file.name || 'image');
    });
});
let settingsPageScrollTop = 0;
let settingsPageScrollPending = false;

document.addEventListener('htmx:before:swap', (event) => {
    const ctx = event.detail?.ctx;
    if (!ctx) {
        return;
    }
    if (ctx.target?.matches?.('#settings-page')) {
        settingsPageScrollTop = ctx.target.scrollTop;
        settingsPageScrollPending = true;
    }
    rememberMessageScroll(ctx.target);
    const swapRoot = ctx.target?.closest?.('.completion-progress') || ctx.target;
    if (swapRoot?.querySelectorAll) pendingExpandedToolCalls.set(ctx.target, snapshotExpandedToolCalls(swapRoot));
    if (ctx.sourceElement?.matches?.('.loading-message') && ctx.response?.status >= 500) {
        event.preventDefault();
        showCompletionNetworkError(event, `Completion failed: the server returned HTTP ${ctx.response.status}. Check the server logs and retry the completion.`);
        return;
    }
    if (Array.isArray(event.detail.tasks)) {
        event.detail.tasks.forEach((task) => {
            const style = task?.swapSpec?.style;
            if (style === 'outerHTML' || style === 'delete') {
                destroyMessageListStates(task.target);
            }
        });
    }
});
document.addEventListener('htmx:after:swap', (event) => {
    const swapCtxTarget = event.detail?.ctx?.target;
    if (swapCtxTarget) {
        const swapRootAfter = swapCtxTarget.closest?.('.completion-progress') || swapCtxTarget;
        if (swapCtxTarget.isConnected !== false) {
            restoreExpandedToolCalls(swapRootAfter, pendingExpandedToolCalls.get(swapCtxTarget));
            pendingExpandedToolCalls.delete(swapCtxTarget);
        }
    }
    restoreMessageScroll(event.detail?.ctx?.target);
    if (settingsPageScrollPending) {
        settingsPageScrollPending = false;
        const settingsPage = document.getElementById('settings-page');
        if (settingsPage) {
            settingsPage.scrollTop = settingsPageScrollTop;
        }
    }
    syncPanelSendButton();
    syncToolResultElisionControls(event.target);
    autoresizeAllMessageInputs(event.target);
});
function applySettingsTheme(root) {
    const settingsPage = root?.matches?.('#settings-page')
        ? root
        : root?.querySelector?.('#settings-page');
    const style = settingsPage?.dataset.themeStyle;
    if (!style) {
        return;
    }

    const rootElement = document.documentElement;
    if (settingsPage.dataset.themeId) {
        rootElement.dataset.theme = settingsPage.dataset.themeId;
    }
    rootElement.setAttribute('style', style);
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta && settingsPage.dataset.themeColor) {
        meta.setAttribute('content', settingsPage.dataset.themeColor);
    }
}

document.addEventListener('htmx:after:settle', (event) => {
    restoreMessageScroll(event.detail?.task?.target);
    autoCopyShareLinks(event.detail?.task?.target ?? event.target);
    initializeShell(event.detail?.task?.target ?? event.target);
    applySettingsTheme(event.detail?.task?.target);
    syncToolResultElisionControls(event.target);
    autoresizeAllMessageInputs(event.target);
    const settingsPage = document.querySelector('#settings-page');
    if (event.detail?.task?.target?.matches?.('#settings-page') && settingsPage?.querySelector('[data-settings-saved]')) {
        const secretInputs = settingsPage.querySelectorAll('#ntfy-api-key, #llm-api-key, input[name^="mcp_authorization_"]');
        secretInputs.forEach((input) => {
            input.value = '';
            input.disabled = false;
        });
    }
});
document.addEventListener('htmx:after:request', (event) => {
    const ctx = event.detail?.ctx;
    const source = ctx?.sourceElement;
    const status = ctx?.response?.status;
    if (status >= 200 && status <= 399 && source?.matches?.('#message-form, .message-edit-form, .history-rename, .history-delete')) {
        refreshChatSummary();
    }
    if (status >= 200 && status <= 399 && source?.matches?.('#message-form')) {
        resetSubmissionID(source);
        const input = source.querySelector('#image-input');
        if (input) {
            clearAttachments(input);
        }
        autoresizeAllMessageInputs(source);
    }
    syncPanelSendButton();
});
document.addEventListener('htmx:finally:request', () => {
    queueMicrotask(syncPanelSendButton);
});
document.addEventListener('kritui:command', (event) => {
    const messageInput = document.querySelector('#message');
    if (messageInput) {
        if (!event.detail?.preserveInput) {
            messageInput.value = '';
            messageInput.style.height = 'auto';
            autoresizeMessageInput(messageInput);
        }
        closeCommandAutocomplete(messageInput);
    }

    const panelID = event.detail?.panel;
    if (!panelID) {
        messageInput?.focus();
        return;
    }
    const panel = document.getElementById(panelID);
    const button = document.querySelector(`[data-panel-target="${CSS.escape(panelID)}"]`);
    if (panel?.hidden) {
        togglePanel(panelID, button);
    }
});
document.addEventListener('kritui:message-edited', () => {
    const messageInput = document.querySelector('#message');
    if (messageInput) {
        messageInput.style.height = 'auto';
        autoresizeMessageInput(messageInput);
        messageInput.focus();
    }
});
document.addEventListener('htmx:before:cleanup', (event) => {
    const root = event.target;
    closeImageViewerWithin(root, false);
    closePdfViewerWithin(root, false);
    clearComposerAttachments(root);
    destroyMessageListStates(root);
    destroyShell(root);
});
document.addEventListener('htmx:sse:before:message', (event) => {
    rememberMessageScroll(event.target);
    if (event.target?.querySelectorAll) {
        pendingExpandedToolCalls.set(event.target, snapshotExpandedToolCalls(event.target));
    }
});
document.addEventListener('htmx:sse:after:message', (event) => {
    restoreExpandedToolCalls(event.target, pendingExpandedToolCalls.get(event.target));
    pendingExpandedToolCalls.delete(event.target);
    restoreMessageScroll(event.target);
    syncPanelSendButton();
    if (!document.querySelector('.completion-active:not(.completion-failed)')) {
        refreshChatSummary();
    }
});
document.addEventListener('htmx:sse:close', (event) => {
    if (event.detail?.reason === 'message') {
        showCompletionNetworkError(event, 'Completion status expired. Retry the completion.');
    }
});
document.addEventListener('htmx:response:error', (event) => {
    const actions = shareActionsFor(event.detail?.ctx?.sourceElement);
    if (actions) {
        setShareStatus(actions, 'Could not create share link. Try Share again.', 'error');
    }
});
document.addEventListener('htmx:error', (event) => {
    const actions = shareActionsFor(event.detail?.ctx?.sourceElement);
    if (actions) {
        setShareStatus(actions, 'Could not create share link. Try Share again.', 'error');
    }
    showCompletionNetworkError(event, 'Could not reach the server. Check your connection and retry the completion.');
});

document.addEventListener('htmx:confirm', (event) => {
    if (event.target.matches('.history-delete') && event.detail.ctx?.sourceEvent?.shiftKey) {
        event.preventDefault();
        event.detail.issueRequest();
    }
});

document.addEventListener('click', (event) => {
    const viewerTrigger = imageViewerTriggerFor(event.target);
    if (viewerTrigger) {
        event.preventDefault();
        openImageViewer(viewerTrigger);
        return;
    }

    const viewerClose = event.target.closest?.('[data-image-viewer-close]');
    if (viewerClose) {
        closeImageViewer();
        return;
    }

    const viewerDialog = event.target.closest?.('#image-viewer');
    if (viewerDialog && event.target === viewerDialog) {
        closeImageViewer();
        return;
    }

    const pdfViewerTrigger = pdfViewerTriggerFor(event.target);
    if (pdfViewerTrigger) {
        if (pdfViewerTrigger instanceof HTMLAnchorElement && !isPlainPrimaryClick(event)) {
            return;
        }
        event.preventDefault();
        openPdfViewer(pdfViewerTrigger);
        return;
    }

    const pdfViewerClose = event.target.closest?.('[data-pdf-viewer-close]');
    if (pdfViewerClose) {
        closePdfViewer();
        return;
    }

    const pdfViewerDialogElement = event.target.closest?.('#pdf-viewer');
    if (pdfViewerDialogElement && event.target === pdfViewerDialogElement) {
        closePdfViewer();
        return;
    }

    const attachButton = event.target.closest('#attach-button');
    if (attachButton) {
        const imageInput = attachButton.closest('form')?.querySelector('#image-input');
        if (imageInput) {
            imageInput.value = '';
            imageInput.click();
        }
        return;
    }

    const clearOpen = event.target.closest('[data-settings-clear-open]');
    if (clearOpen) {
        clearOpen.closest('.settings-section')?.querySelector('[data-settings-clear-dialog]')?.showModal();
        return;
    }

    const clearCancel = event.target.closest('[data-settings-clear-cancel]');
    if (clearCancel) {
        clearCancel.closest('[data-settings-clear-dialog]')?.close();
        return;
    }

    const clearConfirm = event.target.closest('[data-settings-clear-confirm]');
    if (clearConfirm) {
        settingsClearState(clearConfirm, true);
        clearConfirm.closest('[data-settings-clear-dialog]')?.close();
        return;
    }

    const clearUndo = event.target.closest('[data-settings-clear-undo]');
    if (clearUndo) {
        settingsClearState(clearUndo, false);
        return;
    }

    const systemPromptReset = event.target.closest('[data-settings-reset-system-prompt]');
    if (systemPromptReset) {
        resetSystemPrompt(systemPromptReset);
        return;
    }

    const compactionPromptReset = event.target.closest('[data-settings-reset-compaction-prompt]');
    if (compactionPromptReset) {
        resetCompactionPrompt(compactionPromptReset);
        return;
    }

    const removeImage = event.target.closest('.image-preview-remove');
    if (removeImage) {
        const input = removeImage.closest('main')?.querySelector('#image-input');
        const index = Number(removeImage.dataset.imageIndex);
        const state = input && attachmentStates.get(input);
        if (input && state && Number.isInteger(index) && index >= 0 && index < state.files.length) {
            state.files.splice(index, 1);
            renderAttachmentPreviews(input);
            announceAttachments(input, `${state.files.length} attachment${state.files.length === 1 ? '' : 's'} remaining.`);
            input.closest('form')?.querySelector('#message')?.focus();
        }
        return;
    }
    const commandOption = event.target.closest('.command-option:not([hidden])');
    if (commandOption) {
        const messageInput = document.querySelector('#message');
        if (messageInput) {
            activateCommandOption(messageInput, commandOption);
        }
    } else if (!event.target.closest('#message-form')) {
        closeCommandAutocomplete();
    }

    const modelSummary = event.target.closest('.chat-options > summary');
    if (modelSummary) {
        const openPanel = document.querySelector('#page-panel > .panel-page:not([hidden])');
        if (openPanel) {
            event.preventDefault();
            const openPanelButton = document.querySelector(`[data-panel-target="${CSS.escape(openPanel.id)}"]`);
            togglePanel(openPanel.id, openPanelButton);
            const chatOptions = modelSummary.closest('.chat-options');
            if (chatOptions) {
                chatOptions.open = true;
            }
            modelSummary.focus();
        }
    }

    const panelButton = event.target.closest('[data-panel-target]');
    if (panelButton) {
        togglePanel(panelButton.dataset.panelTarget, panelButton);
    }

    const editButton = event.target.closest('.history-edit');
    if (editButton) {
        const entry = editButton.closest('.history-entry');
        entry.classList.add('editing');
        const input = entry.querySelector('.history-rename input');
        input.focus();
        input.select();
    }

    const cancelButton = event.target.closest('.history-cancel');
    if (cancelButton) {
        const entry = cancelButton.closest('.history-entry');
        entry.querySelector('.history-rename').reset();
        entry.classList.remove('editing');
        entry.querySelector('.history-edit').focus();
    }

    const messageEditButton = event.target.closest('.message-edit-toggle');
    if (messageEditButton) {
        if (document.querySelector('.loading-message.completion-active:not(.completion-failed), .message-edit-form.htmx-request')) {
            return;
        }
        document.querySelectorAll('.message.user.editing').forEach((message) => {
            closeMessageEditor(message, false);
        });
        const message = messageEditButton.closest('.message.user');
        message.classList.add('editing');
        messageEditButton.setAttribute('aria-expanded', 'true');
        const input = message.querySelector('.message-edit-input');
        input.focus();
        input.select();
    }

    const messageCopyButton = event.target.closest('.message-copy-button');
    if (messageCopyButton) {
        copyMessage(messageCopyButton);
    }

    const shareCopyButton = event.target.closest('.message-share-copy-button');
    if (shareCopyButton) {
        copyShareLink(shareCopyButton);
    }

    const messageCancelButton = event.target.closest('.message-edit-cancel');
    if (messageCancelButton) {
        closeMessageEditor(messageCancelButton.closest('.message.user'));
    }

    const toolCallToggle = event.target.closest('.tool-call-toggle');
    if (toolCallToggle) {
        const expanded = toolCallToggle.closest('.tool-call').classList.toggle('expanded');
        toolCallToggle.setAttribute('aria-expanded', String(expanded));
    }

    document.querySelectorAll('.model-picker[open], .tool-picker[open], .append-picker[open]').forEach((picker) => {
        if (!picker.contains(event.target)) {
            picker.removeAttribute('open');
        }
    });
});
