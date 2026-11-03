/* probe: small experiments on the Miyoo Mini Plus before the real player.
 *
 *   probe decode  FILE [vdec-depth]   H.264 through MI_VDEC, count frames
 *   probe display FILE                ...and show them via DIVP StretchBuf (times out here)
 *   probe audio   [seconds]           a tone through Onion's audioserver
 *   probe info                        framebuffer and MI versions
 *   probe order FILE                  VDEC output order vs libavcodec
 *   probe sw FILE                     libavcodec software decode speed
 *   probe conv FILE                   NV12 conversion options, timed
 *   probe divp FILE FMT W H MIRROR    VDEC bound to DIVP (does not work here)
 *
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later.
 */
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <linux/fb.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/soundcard.h>
#include <sys/syscall.h>
#include <time.h>
#include <unistd.h>

#include <libavformat/avformat.h>

#include "mi_sys.h"
#include "mi_vdec.h"
#include "mi_divp.h"
#include "mi_gfx.h"

static double now(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec + ts.tv_nsec / 1e9;
}

static double cpu(void)
{
    struct rusage ru;
    getrusage(RUSAGE_SELF, &ru);
    return ru.ru_utime.tv_sec + ru.ru_utime.tv_usec / 1e6 + ru.ru_stime.tv_sec + ru.ru_stime.tv_usec / 1e6;
}

#define CHECK(x) do { MI_S32 r_ = (x); if (r_ != MI_SUCCESS) printf("  %s -> 0x%x\n", #x, (unsigned)r_); } while (0)

/* ---- framebuffer + GFX ---- */
static int fbfd = -1;
static struct fb_var_screeninfo vinfo;
static struct fb_fix_screeninfo finfo;
static MI_PHY stage_phy;
static void *stage_vir;
static int stage_w = 640, stage_h = 480;

static void fb_open(void)
{
    fbfd = (int)syscall(SYS_openat, AT_FDCWD, "/dev/fb0", O_RDWR);
    ioctl(fbfd, FBIOGET_FSCREENINFO, &finfo);
    ioctl(fbfd, FBIOGET_VSCREENINFO, &vinfo);
    printf("fb: %ux%u virt %ux%u bpp %u off %u,%u line %u smem 0x%lx len %u\n", vinfo.xres, vinfo.yres,
           vinfo.xres_virtual, vinfo.yres_virtual, vinfo.bits_per_pixel, vinfo.xoffset, vinfo.yoffset,
           finfo.line_length, finfo.smem_start, finfo.smem_len);
    printf("fb: r %u/%u g %u/%u b %u/%u a %u/%u\n", vinfo.red.offset, vinfo.red.length, vinfo.green.offset,
           vinfo.green.length, vinfo.blue.offset, vinfo.blue.length, vinfo.transp.offset, vinfo.transp.length);
}

static void present(MI_SYS_BufInfo_t *bi, int *buf)
{
    MI_SYS_FrameData_t *fd = &bi->stFrameData;
    MI_DIVP_DirectBuf_t src, dst;
    MI_SYS_WindowRect_t crop;
    memset(&src, 0, sizeof src);
    memset(&dst, 0, sizeof dst);
    src.ePixelFormat = fd->ePixelFormat;
    src.u32Width = fd->u16Width;
    src.u32Height = fd->u16Height;
    src.u32Stride[0] = fd->u32Stride[0];
    src.u32Stride[1] = fd->u32Stride[1];
    src.phyAddr[0] = fd->phyAddr[0];
    src.phyAddr[1] = fd->phyAddr[1];
    crop.u16X = 0;
    crop.u16Y = 0;
    crop.u16Width = fd->u16Width;
    crop.u16Height = fd->u16Height;
    dst.ePixelFormat = E_MI_SYS_PIXEL_FRAME_ARGB8888;
    dst.u32Width = stage_w;
    dst.u32Height = stage_h;
    dst.u32Stride[0] = stage_w * 4;
    dst.phyAddr[0] = stage_phy;
    static int warned;
    MI_S32 r = MI_DIVP_StretchBuf(&src, &crop, &dst);
    if (r != MI_SUCCESS && !warned++) printf("  StretchBuf -> 0x%x\n", (unsigned)r);

    MI_GFX_Surface_t s, d;
    MI_GFX_Rect_t sr, dr;
    MI_GFX_Opt_t opt;
    MI_U16 fence = 0;
    memset(&opt, 0, sizeof opt);
    s.phyAddr = stage_phy;
    s.eColorFmt = E_MI_GFX_FMT_ARGB8888;
    s.u32Width = stage_w;
    s.u32Height = stage_h;
    s.u32Stride = stage_w * 4;
    sr.s32Xpos = 0;
    sr.s32Ypos = 0;
    sr.u32Width = stage_w;
    sr.u32Height = stage_h;
    d.phyAddr = finfo.smem_start + (MI_PHY)(*buf * vinfo.yres) * finfo.line_length;
    d.eColorFmt = E_MI_GFX_FMT_ARGB8888;
    d.u32Width = vinfo.xres;
    d.u32Height = vinfo.yres;
    d.u32Stride = finfo.line_length;
    dr.s32Xpos = 0;
    dr.s32Ypos = 0;
    dr.u32Width = vinfo.xres;
    dr.u32Height = vinfo.yres;
    opt.eRotate = E_MI_GFX_ROTATE_180;
    opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_ONE;
    opt.eDstDfbBldOp = E_MI_GFX_DFB_BLD_ZERO;
    r = MI_GFX_BitBlit(&s, &sr, &d, &dr, &opt, &fence);
    if (r != MI_SUCCESS && warned < 3) { warned++; printf("  BitBlit -> 0x%x\n", (unsigned)r); }
    MI_GFX_WaitAllDone(TRUE, fence);
    vinfo.yoffset = *buf * vinfo.yres;
    ioctl(fbfd, FBIOPAN_DISPLAY, &vinfo);
    *buf ^= 1;
}

/* ---- VDEC ---- */
static int order_n;
static int run_vdec(const char *path, int depth, int show)
{
    AVFormatContext *fc = NULL;
    av_register_all();
    avformat_network_init();
    if (avformat_open_input(&fc, path, NULL, NULL) < 0) { printf("open failed\n"); return 1; }
    avformat_find_stream_info(fc, NULL);
    int vs = av_find_best_stream(fc, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    if (vs < 0) { printf("no video\n"); return 1; }
    AVStream *st = fc->streams[vs];
    AVCodecContext *cc = st->codec;
    printf("video: %s %dx%d extradata %d tb %d/%d fps %d/%d dur %.1f\n", avcodec_get_name(cc->codec_id), cc->width,
           cc->height, cc->extradata_size, st->time_base.num, st->time_base.den, st->avg_frame_rate.num,
           st->avg_frame_rate.den, fc->duration / 1e6);

    double t0 = now(), c0 = cpu(), tt = now();
    CHECK(MI_SYS_Init());
    printf("  MI_SYS_Init %.0f ms\n", (now() - tt) * 1000); tt = now();
    MI_VDEC_InitParam_t ip;
    memset(&ip, 0, sizeof ip);
    ip.bDisableLowLatency = getenv("NOLOWLAT") ? TRUE : FALSE;
    CHECK(MI_VDEC_InitDev(&ip));
    printf("  MI_VDEC_InitDev %.0f ms\n", (now() - tt) * 1000); tt = now();
    MI_VDEC_OutbufLayoutMode_e lm = E_MI_VDEC_OUTBUF_LAYOUT_LINEAR;
    CHECK(MI_VDEC_SetOutputPortLayoutMode(lm));
    MI_VDEC_ChnAttr_t ca;
    memset(&ca, 0, sizeof ca);
    ca.eCodecType = cc->codec_id == AV_CODEC_ID_HEVC ? E_MI_VDEC_CODEC_TYPE_H265 : E_MI_VDEC_CODEC_TYPE_H264;
    ca.u32BufSize = 1024 * 1024;
    ca.u32PicWidth = cc->width;
    ca.u32PicHeight = cc->height;
    ca.eVideoMode = E_MI_VDEC_VIDEO_MODE_FRAME;
    ca.eDpbBufMode = E_MI_VDEC_DPB_MODE_NORMAL;
    ca.stVdecVideoAttr.u32RefFrameNum = 5;
    MI_S32 r = MI_VDEC_CreateChn(0, &ca);
    printf("CreateChn -> 0x%x %.0f ms\n", (unsigned)r, (now() - tt) * 1000); tt = now();
    if (r != MI_SUCCESS) return 1;
    if (getenv("ORDER")) {
        MI_VDEC_ChnParam_t cp;
        memset(&cp, 0, sizeof cp);
        CHECK(MI_VDEC_GetChnParam(0, &cp));
        printf("  chnparam dec %d order %d fmt %d\n", cp.eDecMode, cp.eOutputOrder, cp.eVideoFormat);
        cp.eOutputOrder = E_MI_VDEC_OUTPUT_ORDER_DISPLAY;
        CHECK(MI_VDEC_SetChnParam(0, &cp));
    }
    if (getenv("PLAYBACK")) CHECK(MI_VDEC_SetDisplayMode(0, E_MI_VDEC_DISPLAY_MODE_PLAYBACK));
    CHECK(MI_VDEC_StartChn(0));
    MI_SYS_ChnPort_t port = { E_MI_MODULE_ID_VDEC, 0, 0, 0 };
    CHECK(MI_SYS_SetChnOutputPortDepth(&port, depth, depth + 3));
    printf("init %.0f ms\n", (now() - t0) * 1000);

    int buf = 1;
    double vt0 = 0, fps = av_q2d(st->avg_frame_rate);
    if (show) {
        fb_open();
        CHECK(MI_GFX_Open());
        if (cc->width * 3 > cc->height * 4) { stage_w = 640; stage_h = 640 * cc->height / cc->width & ~1; }
        CHECK(MI_SYS_MMA_Alloc(NULL, stage_w * stage_h * 4, &stage_phy));
        CHECK(MI_SYS_Mmap(stage_phy, stage_w * stage_h * 4, &stage_vir, FALSE));
        printf("stage %dx%d\n", stage_w, stage_h);
    }

    AVPacket pkt;
    long sent = 0, got = 0, fails = 0, outoforder = 0;
    int64_t lastpts = -1;
    int printed = 0, eof = 0;
    double first = 0, tpres = 0;
    while (!eof || got < sent) {
        if (!eof) {
            int e = av_read_frame(fc, &pkt);
            if (e < 0) {
                eof = 1;
                MI_VDEC_VideoStream_t es = { 0 };
                es.bEndOfStream = TRUE;
                es.bEndOfFrame = TRUE;
                MI_VDEC_SendStream(0, &es, 100);
            } else if (pkt.stream_index == vs) {
                MI_VDEC_VideoStream_t es;
                int64_t p = pkt.pts != AV_NOPTS_VALUE ? pkt.pts : pkt.dts;
                es.pu8Addr = pkt.data;
                es.u32Len = pkt.size;
                es.u64PTS = av_rescale_q(p, st->time_base, (AVRational){ 1, 1000000 });
                es.bEndOfFrame = TRUE;
                es.bEndOfStream = FALSE;
                for (;;) {
                    r = MI_VDEC_SendStream(0, &es, 20);
                    if (r == MI_SUCCESS) { sent++; break; }
                    fails++;
                    break; /* drop on failure; counted */
                }
                av_free_packet(&pkt);
            } else {
                av_free_packet(&pkt);
            }
        }
        for (;;) {
            MI_SYS_BufInfo_t bi;
            MI_SYS_BUF_HANDLE h;
            memset(&bi, 0, sizeof bi);
            if (MI_SYS_ChnOutputPortGetBuf(&port, &bi, &h) != MI_SUCCESS) break;
            if (!got) first = now();
            got++;
            if (lastpts >= 0 && (int64_t)bi.u64Pts < lastpts) outoforder++;
            lastpts = bi.u64Pts;
            if (printed < 3) {
                MI_SYS_FrameData_t *fd = &bi.stFrameData;
                printf("frame pts %llu type %d fmt %d tile %d comp %d %ux%u stride %u/%u phy 0x%llx/0x%llx vir %p crop %u,%u %ux%u eos %d\n",
                       (unsigned long long)bi.u64Pts, bi.eBufType, fd->ePixelFormat, fd->eTileMode, fd->eCompressMode,
                       fd->u16Width, fd->u16Height, fd->u32Stride[0], fd->u32Stride[1],
                       (unsigned long long)fd->phyAddr[0], (unsigned long long)fd->phyAddr[1], fd->pVirAddr[0],
                       fd->stContentCropWindow.u16X, fd->stContentCropWindow.u16Y,
                       fd->stContentCropWindow.u16Width, fd->stContentCropWindow.u16Height, bi.bEndOfStream);
                printed++;
            }
            if (order_n && got <= order_n) {
                MI_SYS_FrameData_t *fd = &bi.stFrameData;
                void *y = NULL;
                unsigned long sum = 0;
                if (MI_SYS_Mmap(fd->phyAddr[0], fd->u32Stride[0] * fd->u16Height, &y, FALSE) == MI_SUCCESS) {
                    for (int i = 0; i < fd->u16Height; i += 4) for (int j = 0; j < fd->u16Width; j += 4) sum += ((uint8_t *)y)[i * fd->u32Stride[0] + j];
                    MI_SYS_Munmap(y, fd->u32Stride[0] * fd->u16Height);
                }
                printf("hw %3ld pts %8.3f sum %lu\n", got, bi.u64Pts / 1e6, sum);
            }
            if (show) {
                if (vt0 == 0) vt0 = now();
                double due = vt0 + (got - 1) / (fps > 0 ? fps : 25);
                double w = due - now();
                if (w > 0) usleep((useconds_t)(w * 1e6));
                double tp = now();
                present(&bi, &buf);
                tpres += now() - tp;
            }
            MI_SYS_ChnOutputPortPutBuf(h);
        }
        if (eof && got < sent) {
            static double eof_t;
            if (eof_t == 0) eof_t = now();
            if (now() - eof_t > 2) break;
            usleep(2000);
        }
    }
    double el = now() - first;
    MI_VDEC_ChnStat_t cs;
    memset(&cs, 0, sizeof cs);
    MI_VDEC_GetChnStat(0, &cs);
    printf("sent %ld got %ld sendfail %ld outoforder %ld in %.2f s = %.1f fps, cpu %.2f s (%.0f%% of one core)\n", sent, got,
           fails, outoforder, el, got / el, cpu() - c0, 100 * (cpu() - c0) / (now() - t0));
    if (show) printf("present avg %.2f ms\n", tpres * 1000 / (got ? got : 1));
    printf("stat: recv %u dec %u left %u err %d\n", cs.u32RecvStreamFrames, cs.u32DecodeStreamFrames, cs.u32LeftPics,
           cs.eErrCode);
    double tq = now();
    CHECK(MI_VDEC_StopChn(0));
    CHECK(MI_VDEC_DestroyChn(0));
    CHECK(MI_VDEC_DeInitDev());
    if (show) {
        MI_SYS_Munmap(stage_vir, stage_w * stage_h * 4);
        MI_SYS_MMA_Free(stage_phy);
        MI_SYS_MemsetPa(finfo.smem_start, 0, finfo.smem_len);
        vinfo.yoffset = 0;
        ioctl(fbfd, FBIOPAN_DISPLAY, &vinfo);
        MI_GFX_Close();
    }
    CHECK(MI_SYS_Exit());
    printf("teardown %.0f ms\n", (now() - tq) * 1000);
    avformat_close_input(&fc);
    return 0;
}

/* ---- audio through libpadsp (Onion's /dev/dsp to audioserver shim) ---- */
static int run_audio(double secs)
{
    void *h = dlopen("libpadsp.so", RTLD_NOW);
    if (!h) h = dlopen("/customer/lib/libpadsp.so", RTLD_NOW);
    if (!h) { printf("dlopen: %s\n", dlerror()); return 1; }
    int (*p_open)(const char *, int, ...) = dlsym(h, "open");
    int (*p_ioctl)(int, unsigned long, ...) = dlsym(h, "ioctl");
    ssize_t (*p_write)(int, const void *, size_t) = dlsym(h, "write");
    int (*p_close)(int) = dlsym(h, "close");
    printf("padsp %p open %p ioctl %p write %p close %p\n", h, p_open, p_ioctl, p_write, p_close);
    double t0 = now();
    int fd = p_open("/dev/dsp", O_WRONLY);
    printf("open -> %d (%s) %.0f ms\n", fd, fd < 0 ? strerror(errno) : "ok", (now() - t0) * 1000);
    if (fd < 0) return 1;
    int v = AFMT_S16_LE;
    printf("SETFMT %d -> %d\n", p_ioctl(fd, SNDCTL_DSP_SETFMT, &v), v);
    v = 2;
    printf("CHANNELS %d -> %d\n", p_ioctl(fd, SNDCTL_DSP_CHANNELS, &v), v);
    int rate = 48000;
    printf("SPEED %d -> %d\n", p_ioctl(fd, SNDCTL_DSP_SPEED, &rate), rate);
    v = 44100;
    printf("SPEED(44100) %d -> %d\n", p_ioctl(fd, SNDCTL_DSP_SPEED, &v), v);
    rate = v;
    audio_buf_info bi;
    memset(&bi, 0, sizeof bi);
    printf("GETOSPACE %d -> frags %d fragsize %d bytes %d\n", p_ioctl(fd, SNDCTL_DSP_GETOSPACE, &bi), bi.fragments,
           bi.fragsize, bi.bytes);
    v = -1;
    printf("GETODELAY %d -> %d\n", p_ioctl(fd, SNDCTL_DSP_GETODELAY, &v), v);
    int q = -1;
    printf("FIONREAD(raw) %d -> %d\n", (int)syscall(SYS_ioctl, fd, FIONREAD, &q), q);
    printf("F_GETPIPE_SZ %d\n", fcntl(fd, F_GETPIPE_SZ));
    short buf[1024 * 2];
    long frames = 0, total = (long)(secs * rate);
    double ph = 0, maxw = 0;
    t0 = now();
    while (frames < total) {
        for (int i = 0; i < 1024; i++) {
            short s = (short)(3000 * sin(ph));
            ph += 2 * M_PI * 440 / rate;
            buf[2 * i] = buf[2 * i + 1] = s;
        }
        double tw = now();
        ssize_t n = p_write(fd, buf, sizeof buf);
        tw = now() - tw;
        if (tw > maxw) maxw = tw;
        if (n != (ssize_t)sizeof buf) printf("write -> %zd\n", n);
        frames += 1024;
        if ((frames / 1024) % 50 == 0) {
            q = -1;
            syscall(SYS_ioctl, fd, FIONREAD, &q);
            v = -1;
            p_ioctl(fd, SNDCTL_DSP_GETODELAY, &v);
            printf("t %.3f written %.3f s, queued %d bytes (%.0f ms), odelay %d\n", now() - t0, (double)frames / rate, q,
                   q * 1000.0 / (rate * 4), v);
        }
    }
    printf("wrote %.2f s of audio in %.2f s, max write %.0f ms\n", (double)frames / rate, now() - t0, maxw * 1000);
    double td = now();
    for (int i = 0; i < 400; i++) {
        q = -1;
        syscall(SYS_ioctl, fd, FIONREAD, &q);
        if (q <= 0) break;
        usleep(5000);
    }
    printf("pipe drained in %.0f ms\n", (now() - td) * 1000);
    p_close(fd);
    return 0;
}

static int run_sw(const char *path, int n)
{
    AVFormatContext *fc = NULL;
    av_register_all();
    if (avformat_open_input(&fc, path, NULL, NULL) < 0) return 1;
    avformat_find_stream_info(fc, NULL);
    int vs = av_find_best_stream(fc, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    AVCodecContext *cc = fc->streams[vs]->codec;
    cc->thread_count = 2;
    avcodec_open2(cc, avcodec_find_decoder(cc->codec_id), NULL);
    AVFrame *f = av_frame_alloc();
    AVPacket pkt;
    int k = 0;
    double t0 = now(), c0 = cpu();
    while (av_read_frame(fc, &pkt) >= 0) {
        if (pkt.stream_index == vs) {
            int got = 0;
            avcodec_decode_video2(cc, f, &got, &pkt);
            if (got) {
                k++;
                if (k <= n) {
                    unsigned long sum = 0;
                    for (int i = 0; i < f->height; i += 4) for (int j = 0; j < f->width; j += 4) sum += f->data[0][i * f->linesize[0] + j];
                    printf("sw %3d pts %8.3f sum %lu\n", k, av_frame_get_best_effort_timestamp(f) * av_q2d(fc->streams[vs]->time_base), sum);
                }
            }
        }
        av_free_packet(&pkt);
    }
    printf("sw decoded %d frames in %.2f s = %.1f fps, cpu %.0f%%\n", k, now() - t0, k / (now() - t0), 100 * (cpu() - c0) / (now() - t0));
    return 0;
}

/* VDEC (display order) bound to DIVP; pull DIVP's output. */
static int run_divp(const char *path, int outfmt, int ow, int oh, int mirror)
{
    AVFormatContext *fc = NULL;
    av_register_all();
    if (avformat_open_input(&fc, path, NULL, NULL) < 0) return 1;
    avformat_find_stream_info(fc, NULL);
    int vs = av_find_best_stream(fc, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    AVStream *st = fc->streams[vs];
    AVCodecContext *cc = st->codec;
    double t0 = now(), c0 = cpu(), tt = now();
    CHECK(MI_SYS_Init());
    MI_VDEC_InitParam_t ip;
    memset(&ip, 0, sizeof ip);
    ip.bDisableLowLatency = getenv("LOWLAT") ? FALSE : TRUE;
    CHECK(MI_VDEC_InitDev(&ip));
    MI_VDEC_ChnAttr_t ca;
    memset(&ca, 0, sizeof ca);
    ca.eCodecType = E_MI_VDEC_CODEC_TYPE_H264;
    ca.u32BufSize = 1024 * 1024;
    ca.u32PicWidth = cc->width;
    ca.u32PicHeight = cc->height;
    ca.eVideoMode = E_MI_VDEC_VIDEO_MODE_FRAME;
    ca.stVdecVideoAttr.u32RefFrameNum = 5;
    CHECK(MI_VDEC_CreateChn(0, &ca));
    CHECK(MI_VDEC_StartChn(0));
    MI_DIVP_ChnAttr_t da;
    memset(&da, 0, sizeof da);
    da.u32MaxWidth = cc->width > 1920 ? cc->width : 1920;
    da.u32MaxHeight = cc->height > 1080 ? cc->height : 1080;
    da.eTnrLevel = E_MI_DIVP_TNR_LEVEL_OFF;
    da.eDiType = E_MI_DIVP_DI_TYPE_OFF;
    da.eRotateType = E_MI_SYS_ROTATE_NONE;
    da.bHorMirror = mirror;
    da.bVerMirror = mirror;
    CHECK(MI_DIVP_CreateChn(0, &da));
    MI_DIVP_OutputPortAttr_t oa;
    memset(&oa, 0, sizeof oa);
    oa.u32Width = ow;
    oa.u32Height = oh;
    oa.ePixelFormat = outfmt;
    oa.eCompMode = E_MI_SYS_COMPRESS_MODE_NONE;
    CHECK(MI_DIVP_SetOutputPortAttr(0, &oa));
    CHECK(MI_DIVP_StartChn(0));
    MI_SYS_ChnPort_t vport = { E_MI_MODULE_ID_VDEC, 0, 0, 0 }, dport = { E_MI_MODULE_ID_DIVP, 0, 0, 0 };
    CHECK(MI_SYS_SetChnOutputPortDepth(&vport, 0, 5));
    CHECK(MI_SYS_BindChnPort(&vport, &dport, 30, 30));
    CHECK(MI_SYS_SetChnOutputPortDepth(&dport, 2, 8));
    printf("init %.0f ms\n", (now() - tt) * 1000);
    int show = getenv("SHOW") != NULL, buf = 1;
    if (show) { fb_open(); CHECK(MI_GFX_Open()); }
    AVPacket pkt;
    long sent = 0, got = 0, ooo = 0, inflight = 0;
    int64_t last = -1;
    int eof = 0, printed = 0;
    double first = 0, lastout = now(), tp = 0, fps = av_q2d(st->avg_frame_rate), vt0 = 0;
    while (!eof || now() - lastout < 1.0) {
        if (!eof && inflight < 6) {
            if (av_read_frame(fc, &pkt) < 0) eof = 1;
            else {
                if (pkt.stream_index == vs) {
                    MI_VDEC_VideoStream_t es = { 0 };
                    es.pu8Addr = pkt.data;
                    es.u32Len = pkt.size;
                    es.u64PTS = av_rescale_q(pkt.pts, st->time_base, (AVRational){ 1, 1000000 });
                    es.bEndOfFrame = TRUE;
                    while (MI_VDEC_SendStream(0, &es, 20) != MI_SUCCESS) usleep(1000);
                    sent++;
                    inflight++;
                }
                av_free_packet(&pkt);
            }
        }
        MI_SYS_BufInfo_t bi;
        MI_SYS_BUF_HANDLE h;
        memset(&bi, 0, sizeof bi);
        if (MI_SYS_ChnOutputPortGetBuf(&dport, &bi, &h) == MI_SUCCESS) {
            if (!got) first = now();
            got++;
            inflight--;
            lastout = now();
            if (last >= 0 && (int64_t)bi.u64Pts < last) ooo++;
            last = bi.u64Pts;
            MI_SYS_FrameData_t *fd = &bi.stFrameData;
            if (printed < 3) {
                printed++;
                printf("divp out pts %.3f type %d fmt %d %ux%u stride %u/%u phy 0x%llx\n", bi.u64Pts / 1e6, bi.eBufType,
                       fd->ePixelFormat, fd->u16Width, fd->u16Height, fd->u32Stride[0], fd->u32Stride[1],
                       (unsigned long long)fd->phyAddr[0]);
            }
            if (show && fd->ePixelFormat == E_MI_SYS_PIXEL_FRAME_ARGB8888) {
                if (vt0 == 0) vt0 = now();
                double w = vt0 + (got - 1) / fps - now();
                if (w > 0) usleep((useconds_t)(w * 1e6));
                double t1 = now();
                MI_GFX_Surface_t s = { fd->phyAddr[0], E_MI_GFX_FMT_ARGB8888, fd->u16Width, fd->u16Height, fd->u32Stride[0] };
                MI_GFX_Surface_t d = { finfo.smem_start + (MI_PHY)(buf * vinfo.yres) * finfo.line_length, E_MI_GFX_FMT_ARGB8888, vinfo.xres, vinfo.yres, finfo.line_length };
                MI_GFX_Rect_t sr = { 0, 0, fd->u16Width, fd->u16Height }, dr = { (int)(vinfo.xres - fd->u16Width) / 2, (int)(vinfo.yres - fd->u16Height) / 2, fd->u16Width, fd->u16Height };
                MI_GFX_Opt_t opt;
                MI_U16 fence = 0;
                memset(&opt, 0, sizeof opt);
                opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_ONE;
                opt.eRotate = mirror ? E_MI_GFX_ROTATE_0 : E_MI_GFX_ROTATE_180;
                MI_GFX_BitBlit(&s, &sr, &d, &dr, &opt, &fence);
                MI_GFX_WaitAllDone(TRUE, fence);
                vinfo.yoffset = buf * vinfo.yres;
                ioctl(fbfd, FBIOPAN_DISPLAY, &vinfo);
                buf ^= 1;
                tp += now() - t1;
            }
            MI_SYS_ChnOutputPortPutBuf(h);
        } else {
            usleep(1000);
        }
    }
    double el = lastout - first;
    printf("sent %ld got %ld outoforder %ld in %.2f s = %.1f fps, cpu %.0f%%, present %.2f ms\n", sent, got, ooo, el, got / el,
           100 * (cpu() - c0) / (now() - t0), got ? tp * 1000 / got : 0);
    CHECK(MI_SYS_UnBindChnPort(&vport, &dport));
    CHECK(MI_DIVP_StopChn(0));
    CHECK(MI_DIVP_DestroyChn(0));
    CHECK(MI_VDEC_StopChn(0));
    CHECK(MI_VDEC_DestroyChn(0));
    CHECK(MI_VDEC_DeInitDev());
    if (show) {
        MI_SYS_MemsetPa(finfo.smem_start, 0, finfo.smem_len);
        vinfo.yoffset = 0;
        ioctl(fbfd, FBIOPAN_DISPLAY, &vinfo);
        MI_GFX_Close();
    }
    CHECK(MI_SYS_Exit());
    return 0;
}

/* Offscreen conversion tests on low-latency VDEC frames. */
static int run_conv(const char *path)
{
    AVFormatContext *fc = NULL;
    av_register_all();
    if (avformat_open_input(&fc, path, NULL, NULL) < 0) return 1;
    avformat_find_stream_info(fc, NULL);
    int vs = av_find_best_stream(fc, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    AVStream *st = fc->streams[vs];
    AVCodecContext *cc = st->codec;
    printf("has_b_frames %d\n", cc->has_b_frames);
    CHECK(MI_SYS_Init());
    MI_VDEC_InitParam_t ip;
    memset(&ip, 0, sizeof ip);
    CHECK(MI_VDEC_InitDev(&ip));
    MI_VDEC_ChnAttr_t ca;
    memset(&ca, 0, sizeof ca);
    ca.eCodecType = E_MI_VDEC_CODEC_TYPE_H264;
    ca.u32BufSize = 1024 * 1024;
    ca.u32PicWidth = cc->width;
    ca.u32PicHeight = cc->height;
    ca.eVideoMode = E_MI_VDEC_VIDEO_MODE_FRAME;
    ca.stVdecVideoAttr.u32RefFrameNum = 5;
    CHECK(MI_VDEC_SetOutputPortLayoutMode(E_MI_VDEC_OUTBUF_LAYOUT_LINEAR));
    CHECK(MI_VDEC_CreateChn(0, &ca));
    CHECK(MI_VDEC_StartChn(0));
    MI_SYS_ChnPort_t port = { E_MI_MODULE_ID_VDEC, 0, 0, 0 };
    CHECK(MI_SYS_SetChnOutputPortDepth(&port, 4, 10));
    CHECK(MI_GFX_Open());
    MI_PHY argb, yuyv;
    void *argbv, *yuyvv;
    CHECK(MI_SYS_MMA_Alloc(NULL, 640 * 480 * 4, &argb));
    CHECK(MI_SYS_Mmap(argb, 640 * 480 * 4, &argbv, FALSE));
    CHECK(MI_SYS_MMA_Alloc(NULL, 1280 * 720 * 2, &yuyv));
    CHECK(MI_SYS_Mmap(yuyv, 1280 * 720 * 2, &yuyvv, TRUE));
    AVPacket pkt;
    int got = 0, inflight = 0, eof = 0;
    double t_stretch = 0, t_yuyv = 0, t_gfx = 0, t_cpu = 0, lastout = now();
    long sent = 0;
    MI_S32 rs = -1, rg = -1;
    uint32_t *cpu_out = malloc(640 * 480 * 4);
    while ((!eof || now() - lastout < 0.5) && got < 300) {
        if (!eof && inflight < 6) {
            if (av_read_frame(fc, &pkt) < 0) eof = 1;
            else {
                if (pkt.stream_index == vs) {
                    MI_VDEC_VideoStream_t es = { 0 };
                    es.pu8Addr = pkt.data;
                    es.u32Len = pkt.size;
                    es.u64PTS = av_rescale_q(pkt.pts, st->time_base, (AVRational){ 1, 1000000 });
                    es.bEndOfFrame = TRUE;
                    if (MI_VDEC_SendStream(0, &es, 30) == MI_SUCCESS) { sent++; inflight++; }
                }
                av_free_packet(&pkt);
            }
        }
        MI_SYS_BufInfo_t bi;
        MI_SYS_BUF_HANDLE h;
        memset(&bi, 0, sizeof bi);
        if (MI_SYS_ChnOutputPortGetBuf(&port, &bi, &h) != MI_SUCCESS) { usleep(1000); continue; }
        got++;
        inflight--;
        lastout = now();
        MI_SYS_FrameData_t *fd = &bi.stFrameData;
        int W = fd->u16Width, H = fd->u16Height;
        if (got == 1) printf("frame type %d fmt %d %dx%d stride %u\n", bi.eBufType, fd->ePixelFormat, W, H, fd->u32Stride[0]);
        /* 1: DIVP StretchBuf NV12 -> ARGB 640x480 */
        double t = now();
        MI_DIVP_DirectBuf_t sb = { 0 }, db = { 0 };
        MI_SYS_WindowRect_t crop = { 0, 0, (MI_U16)W, (MI_U16)H };
        sb.ePixelFormat = fd->ePixelFormat; sb.u32Width = W; sb.u32Height = H;
        sb.u32Stride[0] = fd->u32Stride[0]; sb.u32Stride[1] = fd->u32Stride[1];
        sb.phyAddr[0] = fd->phyAddr[0]; sb.phyAddr[1] = fd->phyAddr[1];
        db.ePixelFormat = E_MI_SYS_PIXEL_FRAME_ARGB8888; db.u32Width = 640; db.u32Height = 480;
        db.u32Stride[0] = 640 * 4; db.phyAddr[0] = argb;
        if (got <= 30 && getenv("STRETCH")) { rs = MI_DIVP_StretchBuf(&sb, &crop, &db); t_stretch += now() - t; }
        if (got == 30) printf("StretchBuf -> 0x%x, pixel %08x %08x\n", (unsigned)rs, ((uint32_t *)argbv)[100], ((uint32_t *)argbv)[640 * 240 + 320]);
        /* 2: CPU NV12 -> YUYV, then GFX YUV422 -> ARGB 640x480 */
        void *y = NULL, *uv = NULL;
        MI_U32 ylen = fd->u32Stride[0] * H, uvlen = fd->u32Stride[1] * H / 2;
        MI_SYS_Mmap(fd->phyAddr[0], ylen, &y, TRUE);
        MI_SYS_Mmap(fd->phyAddr[1], uvlen, &uv, TRUE);
        t = now();
        for (int j = 0; j < H; j++) {
            const uint8_t *yr = (uint8_t *)y + j * fd->u32Stride[0], *ur = (uint8_t *)uv + (j / 2) * fd->u32Stride[1];
            uint8_t *o = (uint8_t *)yuyvv + j * W * 2;
            for (int i = 0; i < W; i += 2) { o[0] = yr[i]; o[1] = ur[i]; o[2] = yr[i + 1]; o[3] = ur[i + 1]; o += 4; }
        }
        MI_SYS_FlushInvCache(yuyvv, W * H * 2);
        t_yuyv += now() - t;
        t = now();
        MI_GFX_Surface_t s = { yuyv, E_MI_GFX_FMT_YUV422, (MI_U32)W, (MI_U32)H, (MI_U32)W * 2 };
        MI_GFX_Surface_t d = { argb, E_MI_GFX_FMT_ARGB8888, 640, 480, 640 * 4 };
        MI_GFX_Rect_t sr = { 0, 0, (MI_U32)W, (MI_U32)H }, dr = { 0, 0, 640, 480 };
        MI_GFX_Opt_t opt;
        MI_U16 fence = 0;
        memset(&opt, 0, sizeof opt);
        opt.eSrcDfbBldOp = E_MI_GFX_DFB_BLD_ONE;
        opt.eRotate = getenv("ROT") ? E_MI_GFX_ROTATE_180 : E_MI_GFX_ROTATE_0;
        rg = MI_GFX_BitBlit(&s, &sr, &d, &dr, &opt, &fence);
        MI_GFX_WaitAllDone(TRUE, fence);
        t_gfx += now() - t;
        if (got == 31) printf("GFX YUV422 blit -> 0x%x, pixel %08x %08x\n", (unsigned)rg, ((uint32_t *)argbv)[100], ((uint32_t *)argbv)[640 * 240 + 320]);
        /* 3: CPU NV12 -> ARGB (nearest, 640x480 out) */
        t = now();
        for (int j = 0; j < 480; j++) {
            int sy = j * H / 480;
            const uint8_t *yr = (uint8_t *)y + sy * fd->u32Stride[0], *ur = (uint8_t *)uv + (sy / 2) * fd->u32Stride[1];
            uint32_t *o = cpu_out + j * 640;
            for (int i = 0; i < 640; i++) {
                int sx = i * W / 640;
                int Y = (yr[sx] - 16) * 298, U = ur[sx & ~1] - 128, V = ur[sx | 1] - 128;
                int r = (Y + 409 * V + 128) >> 8, g = (Y - 100 * U - 208 * V + 128) >> 8, b = (Y + 516 * U + 128) >> 8;
                r = r < 0 ? 0 : r > 255 ? 255 : r; g = g < 0 ? 0 : g > 255 ? 255 : g; b = b < 0 ? 0 : b > 255 ? 255 : b;
                o[i] = 0xff000000u | r << 16 | g << 8 | b;
            }
        }
        t_cpu += now() - t;
        if (got == 31) {
            printf("cpu pixel %08x %08x\n", cpu_out[100], cpu_out[640 * 240 + 320]);
            FILE *f = fopen("/tmp/gfx.raw", "wb"); fwrite(argbv, 4, 640 * 480, f); fclose(f);
            f = fopen("/tmp/cpu.raw", "wb"); fwrite(cpu_out, 4, 640 * 480, f); fclose(f);
        }
        MI_SYS_Munmap(y, ylen);
        MI_SYS_Munmap(uv, uvlen);
        MI_SYS_ChnOutputPortPutBuf(h);
    }
    printf("frames %d sent %ld: stretch %.2f ms, nv12->yuyv %.2f ms, gfx yuv422->argb %.2f ms, cpu nv12->argb %.2f ms (per frame)\n", got, sent,
           t_stretch * 1000 / 30, t_yuyv * 1000 / got, t_gfx * 1000 / got, t_cpu * 1000 / got);
    MI_VDEC_StopChn(0);
    MI_VDEC_DestroyChn(0);
    MI_VDEC_DeInitDev();
    MI_GFX_Close();
    MI_SYS_Exit();
    return 0;
}

int main(int argc, char **argv)
{
    setvbuf(stdout, NULL, _IOLBF, 0);
    if (argc >= 3 && !strcmp(argv[1], "decode")) return run_vdec(argv[2], argc > 3 ? atoi(argv[3]) : 4, 0);
    if (argc >= 3 && !strcmp(argv[1], "conv")) return run_conv(argv[2]);
    if (argc >= 7 && !strcmp(argv[1], "divp")) return run_divp(argv[2], atoi(argv[3]), atoi(argv[4]), atoi(argv[5]), atoi(argv[6]));
    if (argc >= 3 && !strcmp(argv[1], "order")) { order_n = 40; return run_vdec(argv[2], 4, 0); }
    if (argc >= 3 && !strcmp(argv[1], "sw")) return run_sw(argv[2], argc > 3 ? atoi(argv[3]) : 40);
    if (argc >= 3 && !strcmp(argv[1], "display")) return run_vdec(argv[2], 4, 1);
    if (argc >= 2 && !strcmp(argv[1], "audio")) return run_audio(argc > 2 ? atof(argv[2]) : 3);
    if (argc >= 2 && !strcmp(argv[1], "info")) {
        fb_open();
        MI_SYS_Version_t v;
        MI_SYS_Init();
        if (MI_SYS_GetVersion(&v) == MI_SUCCESS) printf("mi_sys %s\n", v.u8Version);
        MI_SYS_Exit();
        return 0;
    }
    fprintf(stderr, "usage: probe decode|display FILE | audio [s] | info\n");
    return 2;
}
