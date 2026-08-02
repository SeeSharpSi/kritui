function scrollMessages(root) {
    const messageList = root.matches?.('.message-list')
        ? root
        : root.querySelector?.('.message-list');
    if (messageList) {
        messageList.scrollTop = messageList.scrollHeight;
    }
}

document.addEventListener('DOMContentLoaded', () => scrollMessages(document));
document.addEventListener('htmx:load', (event) => scrollMessages(event.detail.elt));

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
