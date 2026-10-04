/*
 * pryon_wake: runs the device's own libpryon.so wake word engine as a child
 * process of the EchoMuse server.
 *
 * Why a separate process: libpryon.so is Amazon's, reverse engineered, with its
 * own threads and C++ exceptions. A crash in it must not take the server (and
 * with it the microphone and the dashboard terminal) down. The library is
 * dlopen()ed, not linked, so building this needs no Amazon file.
 *
 * Protocol (version 1)
 * --------------------
 * stdin, binary frames, little endian:
 *
 *   'P' 'W' <type:u8> <flags:u8> <len:u32> <len bytes>
 *
 *   type 'A'  audio: S16LE mono 16 kHz, len even (the server sends 2560)
 *   type 'P'  client property, ASCII "name=value" (e.g. "audio_playback=1")
 *   type 'R'  gap in the audio (len 0). Mode -r 0: ignored. Mode -r 1: the
 *             engine is told the audio is discontinuous and warms up again.
 *   EOF       clean shutdown
 *
 * stdout, one text line each, flushed:
 *
 *   READY v=1 warmup=<ms> manifest=<path> attrs=<start of the library's attributes>
 *   W <seq> <keyword> <type> <begin> <end> <score>   accepted wake word
 *   N <keyword> <begin> <end> <score>                near miss (statistics only)
 *   E <code> <text>                                  fatal, followed by exit
 *
 * begin and end are sample indexes of the REAL audio since start: the warm-up
 * noise is subtracted. score is the classifier score from the library's own
 * log line, or -1 when it was not seen.
 *
 * stderr: the library's log lines (level <= 3), plus our own diagnostics.
 *
 * Test mode: -f file.wav [-x speed] feeds a WAV file instead of stdin, at real
 * time speed times -x, and prints the same lines. This is what the first run on
 * a device uses.
 *
 * Exit codes: 0 clean, 3 library or symbol missing, 4 model set, 5 decoder,
 * 6 implausible result (library layout differs), 7 I/O, 2 usage.
 */
#define _GNU_SOURCE
#include <ctype.h>
#include <dlfcn.h>
#include <errno.h>
#include <pthread.h>
#include <signal.h>
#include <stdarg.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/resource.h>
#include <unistd.h>
#if defined(__linux__) || defined(__ANDROID__)
#include <sys/prctl.h>
#endif

#include "pryon_api.h"

#define MODEL_SET "pw-ms"
#define DECODER   "pw-dec"
#define DEFAULT_MANIFEST "/system/local/models/keyword/en-US/ALEXA/pryon.manifest"
#define DEFAULT_LIB      "/system/lib/libpryon.so"
#define RATE        16000
#define MAX_PAYLOAD 65536
#define MAX_PROPS   8
/* The engine must not claim a keyword end further past what we have fed it than
 * this; beyond it the structure offsets are wrong for this library version. */
#define PLAUSIBLE_SLACK 16000

/* ---- library entry points ------------------------------------------------ */

static struct {
    const char *(*GetAttributes)(void);
    int  (*SetLoggingCallback)(PryonLoggingCallback);
    int  (*SetEnumeratedResultCallback)(PryonEnumeratedResultCallback);
    int  (*ModelSetNew)(const char *, const char *, const char *);
    int  (*ModelSetDelete)(const char *);
    void (*FormatDefault)(PryonMultichannelAudioFormat *);
    int  (*NewSpotter)(const char *, const char *, const char *, PryonMultichannelAudioFormat, const char *);
    int  (*PushSamples)(const char *, uint64_t, const int16_t *, uint32_t);
    int  (*SessionEnd)(const char *);
    int  (*PushClientEvents)(const char *, const PryonClientEvent *, uint32_t);
    int  (*DecoderDelete)(const char *);
} api;

/* ---- shared state -------------------------------------------------------- */

static pthread_mutex_t out_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_mutex_t prop_lock = PTHREAD_MUTEX_INITIALIZER;

static const char *want_keyword = "ALEXA";
static atomic_ullong fed;            /* samples handed to the decoder, noise included */
static atomic_ullong noise_total;    /* of which warm-up noise */
static atomic_ullong seq;
static volatile double last_score = -1;
static int opened;

static PryonClientProperty props[MAX_PROPS];
static char prop_names[MAX_PROPS][48];
static unsigned nprops;

/* ---- output -------------------------------------------------------------- */

static void say(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
static void say(const char *fmt, ...)
{
    va_list ap;
    pthread_mutex_lock(&out_lock);
    va_start(ap, fmt);
    int n = vprintf(fmt, ap);
    va_end(ap);
    if (n < 0 || fflush(stdout) != 0) _exit(7);   /* the server is gone */
    pthread_mutex_unlock(&out_lock);
}

static void die(int code, const char *tag, const char *text)
{
    say("E %s %s\n", tag, text);
    _exit(code);
}

/* ---- callbacks ----------------------------------------------------------- */

/* The library's KWS log line carries the classifier score of the decision that
 * produced the next result: "...classifier_score=0.98,active_threshold=0.92...".
 * The result struct has no score field. */
static void on_log(int level, const char *tag, const char *msg)
{
    if (!msg) return;
    const char *s = strstr(msg, "classifier_score=");
    if (s) last_score = strtod(s + strlen("classifier_score="), NULL);
    /* Two harmless fingerprint warnings fire on most accepted wake words. */
    if (level > 3 || strstr(msg, "itmask frame indices")) return;
    fprintf(stderr, "[pryon] %s: %s\n", tag ? tag : "", msg);
}

static void on_result(const char *decoderId, PryonEnumeratedResult *r)
{
    (void)decoderId;
    unsigned long long b = r->beginSampleIndex, e = r->endSampleIndex;
    unsigned long long f = atomic_load(&fed);
    if (!r->keyword || b > e || e > f + PLAUSIBLE_SLACK)
        die(6, "ABI", "implausible result: library layout differs from pryon_api.h");
    if (strcasecmp(r->keyword, want_keyword) != 0) return;   /* STOP and friends */

    unsigned long long nz = atomic_load(&noise_total);
    if (e <= nz) return;                       /* fired inside the warm-up noise */
    b = b > nz ? b - nz : 0;
    e -= nz;
    double score = last_score;
    last_score = -1;

    int t = r->detectionType;
    if (t == PRYON_DETECTION_TYPE_ACCEPT || t == PRYON_DETECTION_TYPE_ACCEPT_FLAGGED)
        say("W %llu %s %d %llu %llu %.3f\n", (unsigned long long)atomic_fetch_add(&seq, 1) + 1,
            r->keyword, t, b, e, score);
    else if (t == PRYON_DETECTION_TYPE_NEAR_MISS)
        say("N %s %llu %llu %.3f\n", r->keyword, b, e, score);
}

/* ---- engine helpers ------------------------------------------------------ */

static void push_props_locked(void)
{
    PryonClientEvent ev[MAX_PROPS];
    for (unsigned i = 0; i < nprops; i++) { ev[i].one = 1; ev[i].property = &props[i]; }
    if (nprops && api.PushClientEvents(DECODER, ev, nprops))
        fprintf(stderr, "pryon_wake: client properties refused\n");
}

static void set_property(const char *name, long long value)
{
    unsigned i;
    pthread_mutex_lock(&prop_lock);
    for (i = 0; i < nprops && strcmp(props[i].name, name); i++) ;
    if (i == nprops && nprops < MAX_PROPS) {
        snprintf(prop_names[i], sizeof prop_names[i], "%s", name);
        props[i].name = prop_names[i];
        props[i]._pad = 0;
        props[i].value = value + 1;           /* forces a push below */
        nprops++;
    }
    if (i < nprops && props[i].value != value) {
        props[i].value = value;
        if (opened) push_props_locked();
    }
    pthread_mutex_unlock(&prop_lock);
}

static void feed(const int16_t *s, uint32_t n)
{
    unsigned long long idx = atomic_load(&fed);
    if (api.PushSamples(DECODER, idx, s, n)) {
        fprintf(stderr, "pryon_wake: push @%llu refused\n", idx);
        return;
    }
    atomic_fetch_add(&fed, n);
}

/* Low level noise before the first real audio. Measured: without about three
 * seconds of lead-in the engine misses a wake word in the first seconds. */
static void warm_up(unsigned ms)
{
    int16_t buf[1280];
    static uint32_t lcg = 12345;
    unsigned long long total = (unsigned long long)ms * RATE / 1000;
    for (unsigned long long done = 0; done < total;) {
        uint32_t n = total - done < 1280 ? (uint32_t)(total - done) : 1280;
        for (uint32_t i = 0; i < n; i++) {
            lcg = lcg * 1664525u + 1013904223u;
            buf[i] = (int16_t)((int)(lcg >> 24) % 97 - 48);   /* about -56 dBFS */
        }
        feed(buf, n);
        atomic_fetch_add(&noise_total, n);
        done += n;
    }
}

static void fail_symbol(const char *name)
{
    char text[128];
    snprintf(text, sizeof text, "missing symbol %s", name);
    die(3, "SYM", text);
}

#define SYM(field, name) do { \
        *(void **)&api.field = dlsym(lib, name); \
        if (!api.field) fail_symbol(name); \
    } while (0)

static void open_engine(const char *libpath, const char *manifest, unsigned warmup_ms)
{
    void *lib = dlopen(libpath, RTLD_NOW);
    if (!lib) {
        char text[256];
        snprintf(text, sizeof text, "%s: %s", libpath, dlerror());
        die(3, "LIB", text);
    }
    SYM(GetAttributes,              "PryonApi_GetAttributes");
    SYM(SetLoggingCallback,         "PryonApi_SetLoggingCallback");
    SYM(SetEnumeratedResultCallback,"PryonApi_SetEnumeratedResultCallback");
    SYM(ModelSetNew,                "PryonModelSet_New");
    SYM(ModelSetDelete,             "PryonModelSet_Delete");
    SYM(FormatDefault,              "PryonDecoder_NewPryonMultichannelAudioFormat_Default");
    SYM(NewSpotter,                 "PryonDecoder_NewSpotterAudioDecoder");
    SYM(PushSamples,                "PryonDecoder_PushAudioEventSamples");
    SYM(SessionEnd,                 "PryonDecoder_SessionEnd");
    SYM(PushClientEvents,           "PryonDecoder_PushClientEvents");
    SYM(DecoderDelete,              "PryonDecoder_Delete");

    const char *attrs = api.GetAttributes();
    api.SetLoggingCallback(on_log);
    api.SetEnumeratedResultCallback(on_result);

    if (api.ModelSetNew(MODEL_SET, manifest, "")) die(4, "MODEL", manifest);
    PryonMultichannelAudioFormat fmt;
    api.FormatDefault(&fmt);
    if (api.NewSpotter(DECODER, MODEL_SET, "pryon", fmt, "{}")) die(5, "DECODER", "NewSpotterAudioDecoder failed");

    pthread_mutex_lock(&prop_lock);
    opened = 1;
    push_props_locked();
    pthread_mutex_unlock(&prop_lock);

    warm_up(warmup_ms);

    char a[121];
    snprintf(a, sizeof a, "%s", attrs ? attrs : "");
    for (char *p = a; *p; p++) if (!isprint((unsigned char)*p) || *p == ' ') *p = '_';
    say("READY v=1 warmup=%u manifest=%s attrs=%s\n", warmup_ms, manifest, a);
}

static void close_engine(void)
{
    if (api.DecoderDelete) api.DecoderDelete(DECODER);
    if (api.ModelSetDelete) api.ModelSetDelete(MODEL_SET);
}

/* ---- input --------------------------------------------------------------- */

static int read_full(int fd, void *buf, size_t n)
{
    unsigned char *p = buf;
    while (n) {
        ssize_t r = read(fd, p, n);
        if (r > 0) { p += r; n -= (size_t)r; continue; }
        if (r == 0) return 0;
        if (errno == EINTR) continue;
        return -1;
    }
    return 1;
}

static int serve_stdin(unsigned warmup_ms, int reset_mode)
{
    static unsigned char payload[MAX_PAYLOAD + 1];
    for (;;) {
        unsigned char h[8];
        int r = read_full(0, h, sizeof h);
        if (r == 0) return 0;                       /* EOF: server closed the pipe */
        if (r < 0) return 7;
        if (h[0] != 'P' || h[1] != 'W') die(7, "IO", "bad frame magic");
        uint32_t len = (uint32_t)h[4] | (uint32_t)h[5] << 8 | (uint32_t)h[6] << 16 | (uint32_t)h[7] << 24;
        if (len > MAX_PAYLOAD) die(7, "IO", "frame too long");
        if (len && read_full(0, payload, len) <= 0) return 7;
        payload[len] = 0;

        switch (h[2]) {
        case 'A':
            if (len & 1) die(7, "IO", "odd audio length");
            feed((const int16_t *)(const void *)payload, len / 2);
            break;
        case 'P': {
            char *eq = strchr((char *)payload, '=');
            if (eq) { *eq = 0; set_property((char *)payload, atoll(eq + 1)); }
            break;
        }
        case 'R':
            if (reset_mode == 1) {
                api.SessionEnd(DECODER);
                pthread_mutex_lock(&prop_lock);
                push_props_locked();
                pthread_mutex_unlock(&prop_lock);
                warm_up(warmup_ms);
            }
            break;
        default:
            break;                                  /* unknown frame types are skipped */
        }
    }
}

/* Test mode: a canonical or chunked WAV, 16 kHz mono S16, at real time * speed. */
static int serve_file(const char *path, double speed)
{
    FILE *f = fopen(path, "rb");
    if (!f) { perror(path); return 7; }
    unsigned char hd[12];
    if (fread(hd, 1, 12, f) != 12 || memcmp(hd, "RIFF", 4) || memcmp(hd + 8, "WAVE", 4)) {
        fprintf(stderr, "pryon_wake: %s is not a WAV file\n", path);
        return 7;
    }
    for (;;) {                                      /* find the data chunk */
        unsigned char ch[8];
        if (fread(ch, 1, 8, f) != 8) { fprintf(stderr, "pryon_wake: no data chunk\n"); return 7; }
        uint32_t sz = ch[4] | ch[5] << 8 | ch[6] << 16 | (uint32_t)ch[7] << 24;
        if (!memcmp(ch, "data", 4)) break;
        fseek(f, (long)sz + (sz & 1), SEEK_CUR);
    }
    int16_t buf[800];
    size_t n;
    while ((n = fread(buf, sizeof buf[0], 800, f)) > 0) {
        feed(buf, (uint32_t)n);
        usleep((useconds_t)(n * 1e6 / RATE / speed));
    }
    fclose(f);
    sleep(2);                                       /* let the decoder thread finish */
    return 0;
}

/* ---- main ---------------------------------------------------------------- */

/* The server's file descriptors must not outlive it in here. In particular the
 * microphone: pcm_open() blocks forever while any process still holds the PCM,
 * so a stray pryon_wake that kept a copy would deafen the next server. */
static void close_inherited_fds(void)
{
    for (int fd = 3; fd < 1024; fd++) close(fd);
}

int main(int argc, char **argv)
{
    const char *lib = DEFAULT_LIB, *manifest = DEFAULT_MANIFEST, *file = NULL;
    unsigned warmup_ms = 3000;
    int reset_mode = 0, niceness = 5, o;
    double speed = 1.0;

    close_inherited_fds();
    signal(SIGPIPE, SIG_IGN);
#if defined(__linux__) || defined(__ANDROID__)
    prctl(PR_SET_PDEATHSIG, SIGKILL);   /* dies with the server, whatever kills it */
#endif

    while ((o = getopt(argc, argv, "l:m:k:w:r:f:x:n:")) != -1) switch (o) {
        case 'l': lib = optarg; break;
        case 'm': manifest = optarg; break;
        case 'k': want_keyword = optarg; break;
        case 'w': warmup_ms = (unsigned)atoi(optarg); break;
        case 'r': reset_mode = atoi(optarg); break;
        case 'f': file = optarg; break;
        case 'x': speed = atof(optarg); if (speed <= 0) speed = 1; break;
        case 'n': niceness = atoi(optarg); break;
        default:
            fprintf(stderr, "usage: pryon_wake [-l libpryon.so] [-m manifest] [-k ALEXA] [-w warmup_ms]\n"
                            "                  [-r 0|1] [-n nice] [-f file.wav [-x speed]]\n");
            return 2;
    }
    /* Parent already gone before PDEATHSIG took effect: nobody to serve. */
    if (!file && getppid() == 1) return 0;
    if (niceness) setpriority(PRIO_PROCESS, 0, niceness);

    open_engine(lib, manifest, warmup_ms);
    int rc = file ? serve_file(file, speed) : serve_stdin(warmup_ms, reset_mode);
    close_engine();
    if (file) fprintf(stderr, "pryon_wake: fed %llu samples (%llu noise), %llu accepted\n",
                      (unsigned long long)atomic_load(&fed), (unsigned long long)atomic_load(&noise_total),
                      (unsigned long long)atomic_load(&seq));
    return rc;
}
