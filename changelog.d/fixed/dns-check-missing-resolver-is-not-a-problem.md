- **A missing resolver file is no longer a problem** ([#296](https://github.com/trendvidia/voya/issues/296)).
  The file is opt-in, so `voya dns check` gives verdict `ok` and exit 0 without
  it, with the state still reading `resolver-not-installed` and the install
  command still named. A probe that could not run gives `unknown`, and anything
  answering `*.internal` without the file, or an installed file that does not
  work, is still `problem`.
