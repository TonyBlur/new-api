#!/bin/bash
ACCESS_TOKEN="ya29.a0Aa7MYioowZjSHnTg-fg1Od_EfjwZiDC9jsuv5t44s4E1DfoQeztGXyLh1amOLzLzgakdZD2dtIwIRTK0YZHz7VmWwLcAMKHmd4NmjKoS9F0M3pw9HWNFuWpzI1tLcNz836g8CqgpEIYM9gLnKRHBOfwIGoy1F6t-nIXEWf7dDjke21JdN0lvvT6pJjTNwx9fx6Aarc-7dPDpaCgYKAcMSARUSFQHGX2MidGzZVttH0T5NpunRjf9kcg0211"

echo "=== Testing model list ==="
wget -S -O - \
  --post-data='{"project":"unique-case-862g6"}' \
  --header="Authorization: Bearer ${ACCESS_TOKEN}" \
  --header="Content-Type: application/json" \
  --header="User-Agent: google-api-nodejs-client/9.15.1" \
  --header="X-Goog-Api-Client: gdcl/9.15.1 gl-node/20.18.2" \
  'https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels' 2>&1

echo ""
echo "=== Testing gemini-3-flash ==="
wget -S -O - \
  --post-data='{"project":"unique-case-862g6","model":"gemini-3-flash","userAgent":"antigravity","requestType":"agent","requestId":"agent-test-123","request":{"contents":[{"role":"user","parts":[{"text":"Hello"}]}],"sessionId":"test-session-123"}}' \
  --header="Authorization: Bearer ${ACCESS_TOKEN}" \
  --header="Content-Type: application/json" \
  --header="User-Agent: google-api-nodejs-client/9.15.1" \
  --header="X-Goog-Api-Client: gdcl/9.15.1 gl-node/20.18.2" \
  --header="Client-Metadata: mode=proactive,source=cloudcode-vscode,extension_version=2.29.0,vscode_version=1.98.2,environment=vscode_cloudshelleditor" \
  'https://cloudcode-pa.googleapis.com/v1internal:generateContent' 2>&1
