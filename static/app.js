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

function activateCommandOption(input, option) {
    input.value = `/${option.dataset.commandName}`;
    closeCommandAutocomplete(input);
    if (option.dataset.commandRequiresArguments === 'true') {
        input.value += ' ';
        input.focus();
        input.setSelectionRange(input.value.length, input.value.length);
        return;
    }

    input.form?.requestSubmit();
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

document.addEventListener('input', (event) => {
    if (event.target.matches('#message')) {
        updateCommandAutocomplete(event.target);
    }
});

document.addEventListener('focusin', (event) => {
    if (event.target.matches('#message')) {
        updateCommandAutocomplete(event.target);
    }
});

document.addEventListener('keydown', (event) => {
    const input = event.target;

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
        const autocomplete = commandAutocompleteFor(input);
        const selected = autocomplete?.hidden
            ? null
            : autocomplete?.querySelector('.command-option[aria-selected="true"]:not([hidden])');
        if (selected) {
            event.preventDefault();
            activateCommandOption(input, selected);
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

let appendPickerSelection = null;

document.addEventListener('htmx:oobBeforeSwap', (event) => {
    if (event.target.id !== 'append-picker') {
        return;
    }
    appendPickerSelection = Array.from(event.target.querySelectorAll('input[name="append"]:checked'))
        .map((input) => input.value);
});

document.addEventListener('htmx:oobAfterSwap', (event) => {
    if (event.target.id !== 'append-picker' || appendPickerSelection === null) {
        return;
    }
    const selection = appendPickerSelection;
    appendPickerSelection = null;
    event.target.querySelectorAll('input[name="append"]').forEach((input) => {
        input.checked = selection.includes(input.value);
    });
});

document.addEventListener('htmx:load', (event) => scrollMessages(event.detail.elt));
document.addEventListener('htmx:beforeSwap', (event) => rememberMessageScroll(event.detail.target));
document.addEventListener('htmx:afterSwap', (event) => {
    restoreMessageScroll(event.detail.elt);
    syncPanelSendButton();
});
document.addEventListener('htmx:afterSettle', (event) => restoreMessageScroll(event.detail.elt));
document.addEventListener('htmx:afterRequest', syncPanelSendButton);
document.addEventListener('kritui:command', (event) => {
    const messageInput = document.querySelector('#message');
    if (messageInput) {
        if (!event.detail?.preserveInput) {
            messageInput.value = '';
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
    document.querySelector('#message')?.focus();
});
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
    const commandOption = event.target.closest('.command-option:not([hidden])');
    if (commandOption) {
        const messageInput = document.querySelector('#message');
        if (messageInput) {
            activateCommandOption(messageInput, commandOption);
        }
    } else if (!event.target.closest('#message-form')) {
        closeCommandAutocomplete();
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
