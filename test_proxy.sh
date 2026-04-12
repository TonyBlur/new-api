#!/bin/sh
export http_proxy=http://host.docker.internal:7897
export https_proxy=http://host.docker.internal:7897
wget -q -O - \
  --post-data='{"project":"unique-case-862g6"}' \
  --header='Authorization: Bearer ya29.a0Aa7MYioowZjSHnTg-fg1Od_EfjwZiDC9jsuv5t44s4E1DfoQeztGXyLh1amOLzLzgakdZD2dtIwIRTK0YZHz7VmWwLcAMKHmd4NmjKoS9F0M3pw9HWNFuWpzI1tLcNz836g8CqgpEIYM9gLnKRHBOfwIGoy1F6t-nIXEWf7dDjke21JdN0lvvT6pJjTNwx9fx6Aarc-7dPDpaCgYKAcMSARUSFQHGX2MidGzZVttH0T5NpunRjf9kcg0211' \
  --header='Content-Type: application/json' \
  https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels
