-- Maintained evidence aggregator for the hufu-dev team's baseline commands.
-- Used by implementation-engineer (side_effect: workspace_write); do not wire
-- this into a side_effect:none agent — the read-only tool-capability gate in
-- internal/team/tool_policy_gate.go always denies the `lua` tool, and its
-- read-only bash grammar denies the "{ ...; echo EXIT:$?; } > file" capture
-- syntax this script depends on, regardless of what this file's code does.
--
-- Usage: before calling `lua`, capture each baseline command's output with
-- its exit code appended as a trailing "EXIT:<code>" line, e.g.:
--   { result=0; mise run test || result=$?; printf '\nEXIT:%s\n' "$result"; } > "$evidence_dir/gotest.log" 2>&1
-- Then call the `lua` tool with a loader that sets INPUTS and executes this
-- file instead of regenerating the parsing/aggregation logic every call:
--   INPUTS = { gotest = "/tmp/gotest.log", govet = "/tmp/govet.log" }
--   local f = io.open(".agent-teams/hufu-dev/evidence-summary.lua", "r")
--   local code = f:read("*a"); f:close()
--   local fn, err = loadstring(code)
--   if not fn then print("load failed: "..tostring(err)); return end
--   fn()
-- Recognized INPUTS keys: gotest, race, govet, lint. All are optional; only
-- the keys present are included in the table.

local function read_file(path)
  local f = io.open(path, "r")
  if not f then return nil end
  local content = f:read("*a")
  f:close()
  return content
end

local function parse_exit(content)
  local code = content:match("\nEXIT:(%-?%d+)%s*$") or content:match("^EXIT:(%-?%d+)%s*$")
  return code and tonumber(code) or nil
end

local function count_go_test(content)
  local packages_passed, packages_failed, packages_without_tests = 0, 0, 0
  local tests_passed, tests_failed = 0, 0
  local failing = {}
  for line in content:gmatch("[^\n]+") do
    if line:match("^ok%s+%S+%s+") then
      packages_passed = packages_passed + 1
    elseif line:match("^FAIL%s+%S+") then
      packages_failed = packages_failed + 1
      failing[#failing + 1] = line
    elseif line:match("^%?%s+%S+%s+%[no test files%]") then
      packages_without_tests = packages_without_tests + 1
    elseif line:match("^%s*%-%-%- PASS:") then
      tests_passed = tests_passed + 1
    elseif line:match("^%s*%-%-%- FAIL:") then
      tests_failed = tests_failed + 1
      failing[#failing + 1] = line
    end
  end
  local summary = "no Go test records"
  if packages_passed + packages_failed + packages_without_tests > 0 then
    summary = string.format("%d packages passed, %d failed, %d without tests",
      packages_passed, packages_failed, packages_without_tests)
  elseif tests_passed + tests_failed > 0 then
    summary = string.format("%d test results passed, %d failed", tests_passed, tests_failed)
  end
  return summary, failing
end

if type(INPUTS) ~= "table" then
  print("evidence-summary.lua: global INPUTS table not set; nothing to summarize")
  return
end

local order = { "gotest", "race", "govet", "lint" }
local label = { gotest = "mise run test", race = "mise exec -- go test -p 2 -race",
  govet = "mise run vet", lint = "mise run lint" }

print("| command | exit | result |")
print("|---|---|---|")

local overall_pass = true
local fail_details = {}

for _, key in ipairs(order) do
  local path = INPUTS[key]
  if path then
    local content = read_file(path)
    if not content then
      print(string.format("| %s | ? | log file not found: %s |", label[key], path))
      overall_pass = false
    else
      local exit_code = parse_exit(content)
      local result
      if key == "gotest" or key == "race" then
        local failing
        result, failing = count_go_test(content)
        if #failing > 0 then
          overall_pass = false
          for _, l in ipairs(failing) do
            fail_details[#fail_details + 1] = "  - " .. l
          end
        end
      else
        result = (exit_code == 0) and "clean" or "see log"
      end
      if exit_code ~= 0 then
        overall_pass = false
      end
      if exit_code == nil then
        result = result .. " (missing or invalid EXIT marker)"
      end
      print(string.format("| %s | %s | %s |", label[key], exit_code or "?", result))
    end
  end
end

print("")
if overall_pass then
  print("VERDICT: PASS")
else
  print("VERDICT: FAIL")
  if #fail_details > 0 then
    print("Failure details:")
    for _, l in ipairs(fail_details) do
      print(l)
    end
  end
end
