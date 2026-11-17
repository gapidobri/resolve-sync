// brawframes decodes frames of a Blackmagic RAW clip to small grayscale
// pictures on standard output, the same form the rest of the app gets from
// ffmpeg for other formats.
//
//   brawframes <clip.braw> <first frame> <count> <step> <width> <height> [rgb]
//
// It writes count pictures of width x height bytes each (three bytes a pixel
// with rgb): frames first, first+step, first+2*step, ... Frames past the end
// of the clip are left out.
//
// The clip is opened for reading only. Nothing is written beside it: no
// sidecar is ever saved.
//
// Building needs the Blackmagic RAW SDK (headers); running needs only the
// Blackmagic RAW library, which DaVinci Resolve already installs. See
// scripts/build-braw.sh.

#include "BlackmagicRawAPI.h"
#include "PlatformHelpers.h"

#include <atomic>
#include <condition_variable>
#include <cstdio>
#include <cstdlib>
#include <mutex>
#include <string>
#include <sys/stat.h>
#include <vector>

namespace {

struct State {
	int outW = 0, outH = 0;
	bool rgb = false;
	std::vector<std::vector<unsigned char>> pictures; // by output index; empty if failed
	std::mutex mu;
	std::condition_variable cv;
	int inFlight = 0;
};

// Area-average an RGBA picture down to a small grayscale (or RGB) one.
std::vector<unsigned char> shrink(const unsigned char* rgba, unsigned w, unsigned h, int outW, int outH, bool rgb) {
	const int channels = rgb ? 3 : 1;
	std::vector<unsigned char> out(static_cast<size_t>(outW) * outH * channels);
	for (int oy = 0; oy < outH; oy++) {
		unsigned y0 = static_cast<unsigned>(static_cast<uint64_t>(oy) * h / outH);
		unsigned y1 = static_cast<unsigned>(static_cast<uint64_t>(oy + 1) * h / outH);
		if (y1 <= y0) y1 = y0 + 1;
		for (int ox = 0; ox < outW; ox++) {
			unsigned x0 = static_cast<unsigned>(static_cast<uint64_t>(ox) * w / outW);
			unsigned x1 = static_cast<unsigned>(static_cast<uint64_t>(ox + 1) * w / outW);
			if (x1 <= x0) x1 = x0 + 1;
			uint64_t sum = 0, r = 0, g = 0, b = 0;
			for (unsigned y = y0; y < y1 && y < h; y++) {
				const unsigned char* p = rgba + (static_cast<size_t>(y) * w + x0) * 4;
				for (unsigned x = x0; x < x1 && x < w; x++, p += 4) {
					sum += 54u * p[0] + 183u * p[1] + 19u * p[2]; // Rec. 709 luma, x256
					r += p[0];
					g += p[1];
					b += p[2];
				}
			}
			uint64_t n = static_cast<uint64_t>(y1 - y0) * (x1 - x0);
			size_t at = (static_cast<size_t>(oy) * outW + ox) * channels;
			if (rgb) {
				out[at] = static_cast<unsigned char>(r / n);
				out[at + 1] = static_cast<unsigned char>(g / n);
				out[at + 2] = static_cast<unsigned char>(b / n);
			} else {
				out[at] = static_cast<unsigned char>(sum / (n * 256));
			}
		}
	}
	return out;
}

class Callback : public IBlackmagicRawCallback {
public:
	explicit Callback(State* s) : state(s) {}

	void done() {
		std::lock_guard<std::mutex> lock(state->mu);
		state->inFlight--;
		state->cv.notify_all();
	}

	void ReadComplete(IBlackmagicRawJob* readJob, HRESULT result, IBlackmagicRawFrame* frame) override {
		void* index = nullptr;
		readJob->GetUserData(&index);
		IBlackmagicRawJob* job = nullptr;
		if (SUCCEEDED(result)) result = frame->SetResolutionScale(blackmagicRawResolutionScaleEighth);
		if (SUCCEEDED(result)) result = frame->SetResourceFormat(blackmagicRawResourceFormatRGBAU8);
		if (SUCCEEDED(result)) result = frame->CreateJobDecodeAndProcessFrame(nullptr, nullptr, &job);
		if (SUCCEEDED(result)) result = job->SetUserData(index);
		if (SUCCEEDED(result)) result = job->Submit();
		if (job) job->Release();
		if (FAILED(result)) done();
	}

	void ProcessComplete(IBlackmagicRawJob* job, HRESULT result, IBlackmagicRawProcessedImage* image) override {
		void* index = nullptr;
		job->GetUserData(&index);
		unsigned w = 0, h = 0;
		void* data = nullptr;
		if (SUCCEEDED(result)) result = image->GetWidth(&w);
		if (SUCCEEDED(result)) result = image->GetHeight(&h);
		if (SUCCEEDED(result)) result = image->GetResource(&data);
		if (SUCCEEDED(result) && data && w && h) {
			auto small = shrink(static_cast<const unsigned char*>(data), w, h, state->outW, state->outH, state->rgb);
			std::lock_guard<std::mutex> lock(state->mu);
			state->pictures[reinterpret_cast<size_t>(index)] = std::move(small);
		}
		done();
	}

	void ReadAudioComplete(IBlackmagicRawJob*, HRESULT, IBlackmagicRawAudioBuffer*) override {}
	void DecodeComplete(IBlackmagicRawJob*, HRESULT) override {}
	void TrimProgress(IBlackmagicRawJob*, float) override {}
	void TrimComplete(IBlackmagicRawJob*, HRESULT) override {}
	void SidecarMetadataParseWarning(IBlackmagicRawClip*, COM::NativeString, uint32_t, COM::NativeString) override {}
	void SidecarMetadataParseError(IBlackmagicRawClip*, COM::NativeString, uint32_t, COM::NativeString) override {}
	void PreparePipelineComplete(void*, HRESULT) override {}

	HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, LPVOID* out) override {
		if (!out) return E_POINTER;
		if (iid == IID_IUnknown || iid == IID_IBlackmagicRawCallback) {
			*out = this;
			AddRef();
			return S_OK;
		}
		*out = nullptr;
		return E_NOINTERFACE;
	}
	ULONG STDMETHODCALLTYPE AddRef() override { return ++refs; }
	ULONG STDMETHODCALLTYPE Release() override {
		const ULONG n = --refs;
		if (n == 0) delete this;
		return n;
	}

private:
	State* state;
	std::atomic<ULONG> refs = {1};
};

bool isDir(const std::string& path) {
	struct stat st;
	return stat(path.c_str(), &st) == 0 && S_ISDIR(st.st_mode);
}

// The library is looked for where the user said, then in the installs that
// ship it: the SDK, DaVinci Resolve and Blackmagic RAW Player.
IBlackmagicRawFactory* loadLibrary() {
	std::vector<std::string> dirs;
	if (const char* env = std::getenv("BRAW_LIBRARY_DIR")) dirs.push_back(env);
	dirs.push_back("/Applications/Blackmagic RAW/Blackmagic RAW SDK/Mac/Libraries");
	dirs.push_back("/Applications/DaVinci Resolve/DaVinci Resolve.app/Contents/Frameworks");
	dirs.push_back("/Applications/Blackmagic RAW/Blackmagic RAW Player.app/Contents/Frameworks");
	for (const auto& dir : dirs) {
		if (!isDir(dir + "/BlackmagicRawAPI.framework")) continue;
		COM::String path(dir.c_str());
		if (IBlackmagicRawFactory* f = CreateBlackmagicRawFactoryInstanceFromPath(path)) return f;
	}
	return nullptr;
}

} // namespace

int main(int argc, const char* argv[]) {
	if (argc != 7 && !(argc == 8 && std::string(argv[7]) == "rgb")) {
		std::fprintf(stderr, "usage: brawframes <clip.braw> <first> <count> <step> <width> <height> [rgb]\n");
		return 2;
	}
	const long long first = std::atoll(argv[2]);
	const long long count = std::atoll(argv[3]);
	const long long step = std::atoll(argv[4]);
	State state;
	state.outW = std::atoi(argv[5]);
	state.outH = std::atoi(argv[6]);
	state.rgb = argc == 8;
	if (first < 0 || count <= 0 || step <= 0 || state.outW <= 0 || state.outH <= 0 || count > 200000) {
		std::fprintf(stderr, "brawframes: invalid arguments\n");
		return 2;
	}

	COM::OwningPtr<IBlackmagicRawFactory> factory(loadLibrary());
	if (factory == nullptr) {
		std::fprintf(stderr, "brawframes: the Blackmagic RAW library was not found (install DaVinci Resolve or Blackmagic RAW, or set BRAW_LIBRARY_DIR)\n");
		return 3;
	}
	COM::OwningPtr<IBlackmagicRaw> codec;
	if (FAILED(factory->CreateCodec(codec.outArg()))) {
		std::fprintf(stderr, "brawframes: could not start the decoder\n");
		return 3;
	}
	COM::OwningPtr<IBlackmagicRawClip> clip;
	COM::String clipName(argv[1]);
	if (FAILED(codec->OpenClip(clipName, clip.outArg()))) {
		std::fprintf(stderr, "brawframes: could not open %s\n", argv[1]);
		return 4;
	}
	uint64_t frames = 0;
	clip->GetFrameCount(&frames);

	COM::OwningPtr<IBlackmagicRawCallback> callback(new Callback(&state));
	if (FAILED(codec->SetCallback(callback))) return 3;

	long long wanted = 0;
	while (wanted < count && static_cast<uint64_t>(first + wanted * step) < frames) wanted++;
	state.pictures.resize(static_cast<size_t>(wanted));

	const int maxInFlight = 8;
	for (long long i = 0; i < wanted; i++) {
		{
			std::unique_lock<std::mutex> lock(state.mu);
			state.cv.wait(lock, [&] { return state.inFlight < maxInFlight; });
			state.inFlight++;
		}
		IBlackmagicRawJob* job = nullptr;
		HRESULT r = clip->CreateJobReadFrame(static_cast<uint64_t>(first + i * step), &job);
		if (SUCCEEDED(r)) r = job->SetUserData(reinterpret_cast<void*>(static_cast<size_t>(i)));
		if (SUCCEEDED(r)) r = job->Submit();
		if (job) job->Release();
		if (FAILED(r)) {
			std::lock_guard<std::mutex> lock(state.mu);
			state.inFlight--;
		}
	}
	codec->FlushJobs();

	// Stop at the first frame that failed, so that what is written is always
	// an unbroken run from the start.
	for (const auto& p : state.pictures) {
		if (p.empty()) break;
		if (std::fwrite(p.data(), 1, p.size(), stdout) != p.size()) return 5;
	}
	std::fflush(stdout);
	return 0;
}
