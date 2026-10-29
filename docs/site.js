for (const button of document.querySelectorAll("[data-copy]")) {
  button.addEventListener("click", async () => {
    const source = document.getElementById(button.dataset.copy);
    const status = document.getElementById("copy-status");
    if (!source) return;
    try {
      await navigator.clipboard.writeText(source.textContent.trim());
      button.textContent = "Copied";
      if (status) status.textContent = "Installation command copied.";
      setTimeout(() => { button.textContent = "Copy"; }, 2000);
    } catch {
      if (status) status.textContent = "Select and copy the command above.";
    }
  });
}

const guide = document.getElementById("guide-navigation");
if (guide) {
  const narrow = window.matchMedia("(max-width: 800px)");
  const syncGuide = () => { guide.open = !narrow.matches; };
  syncGuide();
  narrow.addEventListener("change", syncGuide);
  for (const link of guide.querySelectorAll("a[href^='#']")) {
    link.addEventListener("click", () => { if (narrow.matches) guide.open = false; });
  }
}
