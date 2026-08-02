function scrollMessages(root) {
    const messageList = root.matches?.('.message-list')
        ? root
        : root.querySelector?.('.message-list');
    if (messageList) {
        pinMessageList(messageList, true);
    }
}

const messageListStates = new WeakMap();

function messageListFor(root) {
    return root?.matches?.('.message-list')
        ? root
        : root?.closest?.('.message-list') ?? root?.querySelector?.('.message-list');
}

function isMessageListAtBottom(messageList) {
    return messageList.scrollHeight - messageList.clientHeight - messageList.scrollTop <= 2;
}

function observeMessageBlocks(messageList, state) {
    const blocks = messageList.querySelectorAll('.message, .completed-tool-calls, .chat-empty');
    for (const block of blocks) {
        if (!state.observedBlocks.has(block)) {
            state.observedBlocks.add(block);
            state.resizeObserver.observe(block);
        }
    }
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
        userScrolling: false,
        userScrollTimer: 0,
        observedBlocks: new WeakSet(),
    };
    state.resizeObserver = new ResizeObserver(() => pinMessageList(messageList));
    state.mutationObserver = new MutationObserver(() => {
        observeMessageBlocks(messageList, state);
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

        if (isMessageListAtBottom(messageList)) {
            state.pinned = true;
        } else if (state.userScrolling) {
            state.pinned = false;
        }
    });
    const markUserScrolling = () => {
        state.userScrolling = true;
        clearTimeout(state.userScrollTimer);
        state.userScrollTimer = setTimeout(() => {
            state.userScrolling = false;
        }, 200);
    };
    messageList.addEventListener('wheel', markUserScrolling, { passive: true });
    messageList.addEventListener('touchmove', markUserScrolling, { passive: true });
    messageList.addEventListener('pointerdown', markUserScrolling);
    messageList.addEventListener('pointermove', (event) => {
        if (event.buttons) {
            markUserScrolling();
        }
    });
    observeMessageBlocks(messageList, state);
    messageListStates.set(messageList, state);
    return state;
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

document.addEventListener('DOMContentLoaded', () => scrollMessages(document));
document.addEventListener('htmx:load', (event) => scrollMessages(event.detail.elt));
document.addEventListener('htmx:beforeSwap', (event) => rememberMessageScroll(event.detail.target));
document.addEventListener('htmx:afterSwap', (event) => restoreMessageScroll(event.detail.elt));
document.addEventListener('htmx:afterSettle', (event) => restoreMessageScroll(event.detail.elt));
document.addEventListener('htmx:sseBeforeMessage', (event) => rememberMessageScroll(event.target));
document.addEventListener('htmx:sseMessage', (event) => restoreMessageScroll(event.target));

document.addEventListener('htmx:configRequest', (event) => {
    if (event.detail.elt.matches('.loading-message')) {
        event.detail.parameters.client_timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    }
});

document.addEventListener('htmx:beforeSwap', (event) => {
    const source = event.detail.requestConfig.elt;
    if (event.detail.isError && source?.closest('[data-swap-errors]')) {
        event.detail.shouldSwap = true;
    }
});

document.addEventListener('htmx:confirm', (event) => {
    if (event.target.matches('.history-delete') && event.detail.triggeringEvent?.shiftKey) {
        event.preventDefault();
        event.detail.issueRequest(true);
    }
});

document.addEventListener('click', (event) => {
    document.querySelectorAll('.model-picker[open], .tool-picker[open]').forEach((picker) => {
        if (!picker.contains(event.target)) {
            picker.removeAttribute('open');
        }
    });
});
