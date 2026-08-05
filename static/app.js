function scrollMessages(root) {
    const messageList = root.matches?.('.message-list')
        ? root
        : root.querySelector?.('.message-list');
    if (messageList) {
        pinMessageList(messageList, true);
    }
}

const messageListStates = new WeakMap();
const messageBlockSelector = '.message, .completed-tool-calls, .chat-empty';

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
        observedBlocks: new WeakSet(),
    };
    state.resizeObserver = new ResizeObserver(() => pinMessageList(messageList));
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
        if (state.forcing) {
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
    messageListStates.delete(messageList);
}

function pinMessageList(messageList, force = false) {
    const state = messageListState(messageList);
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

function syncPanelSendButton() {
    const sendButton = document.querySelector('#send-button');
    if (!sendButton) {
        return;
    }

    if (document.querySelector('#page-panel > .panel-page:not([hidden])')) {
        sendButton.dataset.panelDisabled = '';
        sendButton.disabled = true;
        return;
    }
    if (!sendButton.hasAttribute('data-panel-disabled')) {
        return;
    }

    sendButton.removeAttribute('data-panel-disabled');
    const requestActive = document.querySelector('#message-form.htmx-request, .loading-message.htmx-request');
    if (!requestActive) {
        sendButton.disabled = false;
    }
}

function togglePanel(panelID, panelButton) {
    const selectedPanel = document.getElementById(panelID);
    if (!selectedPanel) {
        return;
    }

    const opening = selectedPanel.hidden;
    document.querySelectorAll('#page-panel > .panel-page').forEach((panel) => {
        panel.hidden = !opening || panel !== selectedPanel;
    });
    const chatOptionsSummary = document.querySelector('.chat-options > summary');
    if (chatOptionsSummary) {
        chatOptionsSummary.inert = opening;
        chatOptionsSummary.setAttribute('aria-disabled', String(opening));
    }
    document.querySelectorAll('[data-panel-target]').forEach((button) => {
        const active = opening && button.dataset.panelTarget === panelID;
        button.classList.toggle('active', active);
        button.setAttribute('aria-expanded', String(active));
    });
    if (opening && panelID === 'history-page') {
        selectedPanel.dispatchEvent(new Event('history-open'));
    }
    if (opening) {
        selectedPanel.querySelector('[data-panel-initial-focus]')?.focus();
    } else {
        panelButton?.focus();
    }
    syncPanelSendButton();
}

function showCompletionNetworkError(event, message) {
    const pending = event.detail.elt;
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
    if ('serviceWorker' in navigator) {
        navigator.serviceWorker.register('/sw.js');
    }
});
document.addEventListener('htmx:load', (event) => scrollMessages(event.detail.elt));
document.addEventListener('htmx:beforeSwap', (event) => rememberMessageScroll(event.detail.target));
document.addEventListener('htmx:afterSwap', (event) => {
    restoreMessageScroll(event.detail.elt);
    syncPanelSendButton();
});
document.addEventListener('htmx:afterSettle', (event) => restoreMessageScroll(event.detail.elt));
document.addEventListener('htmx:afterRequest', syncPanelSendButton);
document.addEventListener('htmx:beforeCleanupElement', (event) => {
    const root = event.detail.elt;
    if (root.matches?.('.message-list')) {
        destroyMessageListState(root);
    }
    root.querySelectorAll?.('.message-list').forEach(destroyMessageListState);
});
document.addEventListener('htmx:sseBeforeMessage', (event) => rememberMessageScroll(event.target));
document.addEventListener('htmx:sseMessage', (event) => restoreMessageScroll(event.target));
document.addEventListener('htmx:sendError', (event) => {
    showCompletionNetworkError(event, 'Failed to complete message. Check your connection and retry.');
});
document.addEventListener('htmx:timeout', (event) => {
    showCompletionNetworkError(event, 'Completion request timed out. Retry the completion.');
});

document.addEventListener('htmx:configRequest', (event) => {
    if (event.detail.elt.matches('.loading-message')) {
        event.detail.parameters.client_timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    }
    if (event.detail.elt.matches('.history-loader')) {
        const panelHeight = event.detail.elt.closest('.history-page')?.clientHeight ?? 0;
        event.detail.parameters.limit = Math.min(50, Math.max(1, Math.ceil(panelHeight / 80)));
    }
});

document.addEventListener('htmx:confirm', (event) => {
    if (event.target.matches('.history-delete') && event.detail.triggeringEvent?.shiftKey) {
        event.preventDefault();
        event.detail.issueRequest(true);
    }
});

document.addEventListener('click', (event) => {
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

    const toolCallToggle = event.target.closest('.tool-call-toggle');
    if (toolCallToggle) {
        const expanded = toolCallToggle.closest('.tool-call').classList.toggle('expanded');
        toolCallToggle.setAttribute('aria-expanded', String(expanded));
    }

    document.querySelectorAll('.model-picker[open], .tool-picker[open]').forEach((picker) => {
        if (!picker.contains(event.target)) {
            picker.removeAttribute('open');
        }
    });
});
