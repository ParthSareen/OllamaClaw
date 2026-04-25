-- OllamaClaw local hotkeys for Hammerspoon.
--
-- Defaults:
--   Ctrl+Space: text prompt
--   Ctrl+Shift+Space: hold-to-talk
--
-- macOS often reserves Ctrl+Space for input source switching. If it fires there
-- first, disable or change that shortcut in System Settings, or edit the keys
-- below.
--
-- List ffmpeg microphone devices with:
--   ffmpeg -f avfoundation -list_devices true -i ""
--
-- For avfoundation, ":1" means audio device 1 and no video device. Change
-- audioInput below if your preferred microphone has a different index.

local config = {
  baseURL = "http://127.0.0.1:8790",
  tokenPath = os.getenv("HOME") .. "/.ollamaclaw/local_control.token",
  textMods = { "ctrl" },
  textKey = "space",
  voiceMods = { "ctrl", "shift" },
  voiceKey = "space",
  audioInput = ":1",
  ffmpeg = "/opt/homebrew/bin/ffmpeg",
}

local tokenHeader = "X-OllamaClaw-Local-Token"
local recording = {
  task = nil,
  path = nil,
  stopping = false,
}

local function trim(s)
  return (s or ""):gsub("^%s+", ""):gsub("%s+$", "")
end

local function fileExists(path)
  local f = io.open(path, "r")
  if f then
    f:close()
    return true
  end
  return false
end

if not fileExists(config.ffmpeg) then
  config.ffmpeg = "ffmpeg"
end

local function notify(text)
  hs.notify.new({ title = "OllamaClaw", informativeText = text }):send()
end

local function readToken()
  local f = io.open(config.tokenPath, "r")
  if not f then
    notify("Local control token not found. Start ollamaclaw launch first.")
    return nil
  end
  local token = trim(f:read("*a"))
  f:close()
  if token == "" then
    notify("Local control token is empty.")
    return nil
  end
  return token
end

local function postJSON(path, payload, done)
  local token = readToken()
  if not token then
    return
  end
  hs.http.asyncPost(
    config.baseURL .. path,
    hs.json.encode(payload),
    {
      ["Content-Type"] = "application/json",
      [tokenHeader] = token,
    },
    function(status, body)
      if status >= 200 and status < 300 then
        if done then
          done()
        end
        return
      end
      notify("Request failed: HTTP " .. tostring(status) .. " " .. trim(body))
    end
  )
end

local function sendTextPrompt()
  local button, text = hs.dialog.textPrompt("OllamaClaw", "Send a local prompt", "", "Send", "Cancel")
  text = trim(text)
  if button ~= "Send" or text == "" then
    return
  end
  postJSON("/local/turn", { text = text, source = "hotkey_text" }, function()
    notify("Prompt sent")
  end)
end

local function tempWavPath()
  return hs.fs.temporaryDirectory() .. "/ollamaclaw-" .. tostring(os.time()) .. "-" .. tostring(math.random(100000, 999999)) .. ".wav"
end

local function uploadVoice(path)
  local token = readToken()
  if not token then
    os.remove(path)
    return
  end
  local args = {
    "-sS",
    "-X",
    "POST",
    "-H",
    tokenHeader .. ": " .. token,
    "-F",
    "source=hotkey_voice",
    "-F",
    "audio=@" .. path,
    config.baseURL .. "/local/audio",
  }
  hs.task.new("/usr/bin/curl", function(exitCode, stdOut, stdErr)
    os.remove(path)
    if exitCode == 0 then
      notify("Voice sent")
      return
    end
    notify("Voice upload failed: " .. trim(stdErr ~= "" and stdErr or stdOut))
  end, args):start()
end

local function startRecording()
  if recording.task then
    return
  end
  local path = tempWavPath()
  local args = {
    "-y",
    "-hide_banner",
    "-loglevel",
    "error",
    "-f",
    "avfoundation",
    "-i",
    config.audioInput,
    "-ac",
    "1",
    "-ar",
    "16000",
    "-c:a",
    "pcm_s16le",
    path,
  }
  local task = hs.task.new(config.ffmpeg, function(exitCode, stdOut, stdErr)
    local shouldUpload = recording.stopping
    local finishedPath = recording.path
    recording.task = nil
    recording.path = nil
    recording.stopping = false
    if shouldUpload and finishedPath and fileExists(finishedPath) then
      uploadVoice(finishedPath)
      return
    end
    if finishedPath then
      os.remove(finishedPath)
    end
    notify("Recording failed: " .. trim(stdErr ~= "" and stdErr or stdOut or tostring(exitCode)))
  end, args)
  recording.task = task
  recording.path = path
  recording.stopping = false
  if not task:start() then
    recording.task = nil
    recording.path = nil
    notify("Could not start ffmpeg")
    return
  end
  notify("Recording...")
end

local function stopRecording()
  if not recording.task then
    return
  end
  recording.stopping = true
  recording.task:terminate()
end

hs.hotkey.bind(config.textMods, config.textKey, sendTextPrompt)
hs.hotkey.bind(config.voiceMods, config.voiceKey, startRecording, stopRecording)

notify("OllamaClaw hotkeys loaded")
