/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// A libhailort stand-in for the slice of the HailoRT 5.x genai API that
// hailo-ollama uses, built against HailoRT's own public headers. It emulates
// a Hailo-10H without the device:
//
//   HAILO_SIM_DEVICE=0              VDevice creation fails (no HAT present).
//   HAILO_SIM_DEVICE_MEMORY=<bytes> Device memory; a HEF larger than this
//                                   fails to load. Default 8 GiB.
//   HAILO_SIM_TOKEN_DELAY_MS=<ms>   Delay per generated token. Default 0.
//
// Generation is deterministic: "hailo-sim: " followed by the last user
// message's words, one token per word, cut at max_generated_tokens.

#include "hailo/genai/llm/llm.hpp"
#include "hailo/hailort_defaults.hpp"
#include "hailo/vdevice.hpp"

#include <chrono>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <filesystem>
#include <sstream>
#include <thread>

namespace hailort {

namespace {

uint64_t env_u64(const char *name, uint64_t fallback)
{
    const char *v = std::getenv(name);
    if (v == nullptr || *v == '\0') {
        return fallback;
    }
    return std::strtoull(v, nullptr, 10);
}

// Last "content" string of the last message that has role "user". The
// messages are JSON objects as strings; only \" and \\ escapes are decoded.
std::string last_user_content(const std::vector<std::string> &messages)
{
    for (auto it = messages.rbegin(); it != messages.rend(); ++it) {
        if (it->find("\"user\"") == std::string::npos) {
            continue;
        }
        const auto key = it->find("\"content\"");
        if (key == std::string::npos) {
            continue;
        }
        auto pos = it->find('"', it->find(':', key) + 1);
        if (pos == std::string::npos) {
            continue;
        }
        std::string out;
        for (++pos; pos < it->size() && (*it)[pos] != '"'; ++pos) {
            if ((*it)[pos] == '\\' && pos + 1 < it->size()) {
                ++pos;
                out += ((*it)[pos] == 'n') ? ' ' : (*it)[pos];
                continue;
            }
            out += (*it)[pos];
        }
        return out;
    }
    return "";
}

} // namespace

hailo_vdevice_params_t HailoRTDefaults::get_vdevice_params()
{
    hailo_vdevice_params_t params{};
    params.device_count = 1;
    return params;
}

Expected<std::shared_ptr<VDevice>> VDevice::create_shared(const hailo_vdevice_params_t &)
{
    if (env_u64("HAILO_SIM_DEVICE", 1) == 0) {
        return make_unexpected(HAILO_OUT_OF_PHYSICAL_DEVICES);
    }
    // LLM::create below never dereferences the device.
    return std::shared_ptr<VDevice>();
}

namespace genai {

LLMParams::LLMParams(const std::string &hef_path, const std::string &lora_name, bool optimize_memory_on_device) :
    m_hef_path(hef_path), m_lora(lora_name), m_optimize_memory_on_device(optimize_memory_on_device)
{}

hailo_status LLMParams::set_model(const std::string &hef_path, const std::string &lora_name)
{
    m_hef_path = hef_path;
    m_lora = lora_name;
    return HAILO_SUCCESS;
}

const std::string &LLMParams::hef() const { return m_hef_path; }
const std::string &LLMParams::lora() const { return m_lora; }
bool LLMParams::optimize_memory_on_device() const { return m_optimize_memory_on_device; }
void LLMParams::set_optimize_memory_on_device(bool v) { m_optimize_memory_on_device = v; }

hailo_status LLMGeneratorParams::set_temperature(float32_t v) { m_temperature = v; return HAILO_SUCCESS; }
float32_t LLMGeneratorParams::temperature() const { return m_temperature; }
hailo_status LLMGeneratorParams::set_top_p(float32_t v) { m_top_p = v; return HAILO_SUCCESS; }
float32_t LLMGeneratorParams::top_p() const { return m_top_p; }
hailo_status LLMGeneratorParams::set_top_k(uint32_t v) { m_top_k = v; return HAILO_SUCCESS; }
uint32_t LLMGeneratorParams::top_k() const { return m_top_k; }
hailo_status LLMGeneratorParams::set_frequency_penalty(float32_t v) { m_frequency_penalty = v; return HAILO_SUCCESS; }
float32_t LLMGeneratorParams::frequency_penalty() const { return m_frequency_penalty; }
hailo_status LLMGeneratorParams::set_max_generated_tokens(uint32_t v) { m_max_generated_tokens = v; return HAILO_SUCCESS; }
uint32_t LLMGeneratorParams::max_generated_tokens() const { return m_max_generated_tokens; }
hailo_status LLMGeneratorParams::set_do_sample(bool v) { m_do_sample = v; return HAILO_SUCCESS; }
bool LLMGeneratorParams::do_sample() const { return m_do_sample; }
hailo_status LLMGeneratorParams::set_seed(uint32_t v) { m_seed = v; return HAILO_SUCCESS; }
uint32_t LLMGeneratorParams::seed() const { return m_seed; }

class LLMGeneratorCompletion::Impl
{
public:
    Impl(std::deque<std::string> tokens, bool truncated, std::chrono::milliseconds delay) :
        m_tokens(std::move(tokens)), m_truncated(truncated), m_delay(delay)
    {}

    Expected<std::string> read()
    {
        if (m_tokens.empty()) {
            return make_unexpected(HAILO_INVALID_OPERATION);
        }
        std::this_thread::sleep_for(m_delay);
        auto token = std::move(m_tokens.front());
        m_tokens.pop_front();
        return token;
    }

    Status status() const
    {
        if (m_aborted) {
            return Status::ABORTED;
        }
        if (!m_tokens.empty()) {
            return Status::GENERATING;
        }
        return m_truncated ? Status::MAX_TOKENS_REACHED : Status::LOGICAL_END_OF_GENERATION;
    }

    void abort() { m_aborted = true; m_tokens.clear(); }

private:
    std::deque<std::string> m_tokens;
    bool m_truncated;
    std::chrono::milliseconds m_delay;
    bool m_aborted = false;
};

LLMGeneratorCompletion::LLMGeneratorCompletion(std::unique_ptr<Impl> pimpl) : m_pimpl(std::move(pimpl)) {}
LLMGeneratorCompletion::LLMGeneratorCompletion(LLMGeneratorCompletion &&) = default;
LLMGeneratorCompletion::~LLMGeneratorCompletion() = default;

Expected<std::string> LLMGeneratorCompletion::read(std::chrono::milliseconds) { return m_pimpl->read(); }

Expected<std::string> LLMGeneratorCompletion::read_all(std::chrono::milliseconds)
{
    std::string out;
    while (m_pimpl->status() == Status::GENERATING) {
        auto token = m_pimpl->read();
        if (!token) {
            return make_unexpected(token.status());
        }
        out += token.value();
    }
    return out;
}

Expected<size_t> LLMGeneratorCompletion::read(char *output, size_t output_size, std::chrono::milliseconds)
{
    auto token = m_pimpl->read();
    if (!token) {
        return make_unexpected(token.status());
    }
    const auto n = std::min(output_size, token.value().size());
    std::memcpy(output, token.value().data(), n);
    return size_t(n);
}

LLMGeneratorCompletion::Status LLMGeneratorCompletion::generation_status() const { return m_pimpl->status(); }

hailo_status LLMGeneratorCompletion::abort()
{
    m_pimpl->abort();
    return HAILO_SUCCESS;
}

class LLM::Impl
{
public:
    std::string recovery_sequence = "<|im_end|>";
};

Expected<LLM> LLM::create(std::shared_ptr<VDevice>, const LLMParams &llm_params)
{
    std::error_code ec;
    const auto size = std::filesystem::file_size(llm_params.hef(), ec);
    if (ec) {
        return make_unexpected(HAILO_NOT_FOUND);
    }
    if (size > env_u64("HAILO_SIM_DEVICE_MEMORY", uint64_t(8) << 30)) {
        // The status a real Hailo-10H returns for a HEF that does not fit is unverified.
        return make_unexpected(HAILO_OUT_OF_HOST_MEMORY);
    }
    return LLM(std::make_unique<Impl>());
}

LLM::LLM(std::unique_ptr<Impl> pimpl) : m_pimpl(std::move(pimpl)) {}
LLM::LLM(LLM &&) = default;
LLM::~LLM() = default;

Expected<LLMGeneratorParams> LLM::create_generator_params()
{
    return LLMGeneratorParams(0.7f, 0.8f, 20, 1.1f, 512, true, 0);
}

Expected<LLMGeneratorCompletion> LLM::generate(const std::vector<std::string> &prompt_json_strings,
    const std::vector<std::string> &tools_json_strings)
{
    auto params = create_generator_params();
    return generate(params.value(), prompt_json_strings, tools_json_strings);
}

Expected<LLMGeneratorCompletion> LLM::generate(const LLMGeneratorParams &params,
    const std::vector<std::string> &prompt_json_strings, const std::vector<std::string> &)
{
    std::deque<std::string> tokens{"hailo-sim:"};
    std::istringstream words(last_user_content(prompt_json_strings));
    for (std::string w; words >> w;) {
        tokens.push_back(" " + w);
    }
    const auto max = params.max_generated_tokens();
    const bool truncated = (max > 0) && (tokens.size() > max);
    if (truncated) {
        tokens.resize(max);
    }
    const std::chrono::milliseconds delay(env_u64("HAILO_SIM_TOKEN_DELAY_MS", 0));
    return LLMGeneratorCompletion(std::make_unique<LLMGeneratorCompletion::Impl>(std::move(tokens), truncated, delay));
}

hailo_status LLM::clear_context() { return HAILO_SUCCESS; }

hailo_status LLM::set_generation_recovery_sequence(const std::string &sequence)
{
    m_pimpl->recovery_sequence = sequence;
    return HAILO_SUCCESS;
}

Expected<std::string> LLM::get_generation_recovery_sequence() { return std::string(m_pimpl->recovery_sequence); }

Expected<std::string> LLM::prompt_template()
{
    return std::string(
        "{%- if tools %}<|im_start|>system\n# Tools\n{%- for tool in tools %}{{ tool | tojson }}{%- endfor %}"
        "<|im_end|>\n{%- endif %}"
        "{%- for message in messages %}<|im_start|>{{ message.role }}\n{{ message.content }}<|im_end|>\n"
        "{%- endfor %}{%- if add_generation_prompt %}<|im_start|>assistant\n{%- endif %}");
}

} // namespace genai
} // namespace hailort
