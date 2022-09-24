package storegateway

import (
	"context"
	"fmt"
	vmlibstorage "github.com/VictoriaMetrics/VictoriaMetrics/lib/storage"
	"github.com/go-kit/log/level"
	"github.com/gogo/protobuf/types"
	"github.com/grafana/mimir/pkg/storage/sharding"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb/hashcache"
	"github.com/thanos-io/thanos/pkg/store/hintspb"
	"github.com/thanos-io/thanos/pkg/store/labelpb"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/tracing"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"sync"
	"time"
)

func TagFiltersToPromMatchers(tfss []*vmlibstorage.TagFilters) ([]*labels.Matcher, error) {
	res := make([]*labels.Matcher, 0, len(tfss))
	for _, m := range ms {
		var t labels.MatchType

		switch m.Type {
		case LabelMatcher_EQ:
			t = labels.MatchEqual
		case LabelMatcher_NEQ:
			t = labels.MatchNotEqual
		case LabelMatcher_RE:
			t = labels.MatchRegexp
		case LabelMatcher_NRE:
			t = labels.MatchNotRegexp
		default:
			return nil, errors.Errorf("unrecognized label matcher type %d", m.Type)
		}
		m, err := labels.NewMatcher(t, m.Name, m.Value)
		if err != nil {
			return nil, err
		}
		res = append(res, m)
	}
	return res, nil
}

func (s *BucketStore) SearchMetricNames(tfss []*vmlibstorage.TagFilters, tr vmlibstorage.TimeRange, maxMetrics int, deadline uint64) ([]vmlibstorage.MetricName, error) {
	var err error
	ctx := context.Background()
	if s.queryGate != nil {
		tracing.DoInSpan(ctx, "store_query_gate_ismyturn", func(ctx context.Context) {
			err = s.queryGate.Start(ctx)
		})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to wait for turn")
		}

		defer s.queryGate.Done()
	}

	matchers, err := storepb.MatchersToPromMatchers(req.Matchers...)
	if err != nil {
		return nil, err
	}

	// Check if matchers include the query shard selector.
	shardSelector, matchers, err := sharding.RemoveShardFromMatchers(matchers)
	if err != nil {
		return status.Error(codes.InvalidArgument, errors.Wrap(err, "parse query sharding label").Error())
	}

	var (
		stats            = &queryStats{}
		res              []storepb.SeriesSet
		mtx              sync.Mutex
		g, gctx          = errgroup.WithContext(ctx)
		resHints         = &hintspb.SeriesResponseHints{}
		reqBlockMatchers []*labels.Matcher
		chunksLimiter    = s.chunksLimiterFactory(s.metrics.queriesDropped.WithLabelValues("chunks"))
		seriesLimiter    = s.seriesLimiterFactory(s.metrics.queriesDropped.WithLabelValues("series"))
	)

	if req.Hints != nil {
		reqHints := &hintspb.SeriesRequestHints{}
		if err := types.UnmarshalAny(req.Hints, reqHints); err != nil {
			return status.Error(codes.InvalidArgument, errors.Wrap(err, "unmarshal series request hints").Error())
		}

		reqBlockMatchers, err = storepb.MatchersToPromMatchers(reqHints.BlockMatchers...)
		if err != nil {
			return status.Error(codes.InvalidArgument, errors.Wrap(err, "translate request hints labels matchers").Error())
		}
	}

	gspan, gctx := tracing.StartSpan(gctx, "bucket_store_preload_all")

	s.mtx.RLock()

	for _, bs := range s.blockSets {
		blockMatchers, ok := bs.labelMatchers(matchers...)
		if !ok {
			continue
		}

		blocks := bs.getFor(req.MinTime, req.MaxTime, req.MaxResolutionWindow, reqBlockMatchers)

		if s.debugLogging {
			debugFoundBlockSetOverview(s.logger, req.MinTime, req.MaxTime, req.MaxResolutionWindow, bs.labels, blocks)
		}

		for _, b := range blocks {
			b := b

			if s.enableSeriesResponseHints {
				// Keep track of queried blocks.
				resHints.AddQueriedBlock(b.meta.ULID)
			}

			var chunkr *bucketChunkReader
			// We must keep the readers open until all their data has been sent.
			indexr := b.indexReader()
			if !req.SkipChunks {
				chunkr = b.chunkReader(gctx)
				defer runutil.CloseWithLogOnErr(s.logger, chunkr, "series block")
			}

			// Defer all closes to the end of Series method.
			defer runutil.CloseWithLogOnErr(s.logger, indexr, "series block")

			// If query sharding is enabled we have to get the block-specific series hash cache
			// which is used by blockSeries().
			var blockSeriesHashCache *hashcache.BlockSeriesHashCache
			if shardSelector != nil {
				blockSeriesHashCache = s.seriesHashCache.GetBlockCache(b.meta.ULID.String())
			}

			g.Go(func() error {
				part, pstats, err := blockSeries(
					gctx,
					indexr,
					chunkr,
					blockMatchers,
					shardSelector,
					blockSeriesHashCache,
					chunksLimiter,
					seriesLimiter,
					req.SkipChunks,
					req.MinTime, req.MaxTime,
					req.Aggregates,
					s.logger,
				)
				if err != nil {
					return errors.Wrapf(err, "fetch series for block %s", b.meta.ULID)
				}

				mtx.Lock()
				res = append(res, part)
				stats = stats.merge(pstats)
				mtx.Unlock()

				return nil
			})
		}
	}

	s.mtx.RUnlock()

	defer func() {
		s.metrics.seriesDataTouched.WithLabelValues("postings").Observe(float64(stats.postingsTouched))
		s.metrics.seriesDataFetched.WithLabelValues("postings").Observe(float64(stats.postingsFetched))
		s.metrics.seriesDataSizeTouched.WithLabelValues("postings").Observe(float64(stats.postingsTouchedSizeSum))
		s.metrics.seriesDataSizeFetched.WithLabelValues("postings").Observe(float64(stats.postingsFetchedSizeSum))
		s.metrics.seriesDataTouched.WithLabelValues("series").Observe(float64(stats.seriesTouched))
		s.metrics.seriesDataFetched.WithLabelValues("series").Observe(float64(stats.seriesFetched))
		s.metrics.seriesDataSizeTouched.WithLabelValues("series").Observe(float64(stats.seriesTouchedSizeSum))
		s.metrics.seriesDataSizeFetched.WithLabelValues("series").Observe(float64(stats.seriesFetchedSizeSum))
		s.metrics.seriesDataTouched.WithLabelValues("chunks").Observe(float64(stats.chunksTouched))
		s.metrics.seriesDataFetched.WithLabelValues("chunks").Observe(float64(stats.chunksFetched))
		s.metrics.seriesDataSizeTouched.WithLabelValues("chunks").Observe(float64(stats.chunksTouchedSizeSum))
		s.metrics.seriesDataSizeFetched.WithLabelValues("chunks").Observe(float64(stats.chunksFetchedSizeSum))
		s.metrics.resultSeriesCount.Observe(float64(stats.mergedSeriesCount))
		s.metrics.cachedPostingsCompressions.WithLabelValues(labelEncode).Add(float64(stats.cachedPostingsCompressions))
		s.metrics.cachedPostingsCompressions.WithLabelValues(labelDecode).Add(float64(stats.cachedPostingsDecompressions))
		s.metrics.cachedPostingsCompressionErrors.WithLabelValues(labelEncode).Add(float64(stats.cachedPostingsCompressionErrors))
		s.metrics.cachedPostingsCompressionErrors.WithLabelValues(labelDecode).Add(float64(stats.cachedPostingsDecompressionErrors))
		s.metrics.cachedPostingsCompressionTimeSeconds.WithLabelValues(labelEncode).Add(stats.cachedPostingsCompressionTimeSum.Seconds())
		s.metrics.cachedPostingsCompressionTimeSeconds.WithLabelValues(labelDecode).Add(stats.cachedPostingsDecompressionTimeSum.Seconds())
		s.metrics.cachedPostingsOriginalSizeBytes.Add(float64(stats.cachedPostingsOriginalSizeSum))
		s.metrics.cachedPostingsCompressedSizeBytes.Add(float64(stats.cachedPostingsCompressedSizeSum))
		s.metrics.seriesHashCacheRequests.Add(float64(stats.seriesHashCacheRequests))
		s.metrics.seriesHashCacheHits.Add(float64(stats.seriesHashCacheHits))

		level.Debug(s.logger).Log("msg", "stats query processed",
			"stats", fmt.Sprintf("%+v", stats), "err", err)
	}()

	// Concurrently get data from all blocks.
	{
		begin := time.Now()
		err = g.Wait()
		gspan.Finish()
		if err != nil {
			code := codes.Aborted
			if s, ok := status.FromError(errors.Cause(err)); ok {
				code = s.Code()
			}
			return status.Error(code, err.Error())
		}
		stats.blocksQueried = len(res)
		stats.getAllDuration = time.Since(begin)
		s.metrics.seriesGetAllDuration.Observe(stats.getAllDuration.Seconds())
		s.metrics.seriesBlocksQueried.Observe(float64(stats.blocksQueried))
	}
	// Merge the sub-results from each selected block.
	tracing.DoInSpan(ctx, "bucket_store_merge_all", func(ctx context.Context) {
		begin := time.Now()

		// NOTE: We "carefully" assume series and chunks are sorted within each SeriesSet. This should be guaranteed by
		// blockSeries method. In worst case deduplication logic won't deduplicate correctly, which will be accounted later.
		set := storepb.MergeSeriesSets(res...)
		for set.Next() {
			var series storepb.Series

			stats.mergedSeriesCount++

			var lset labels.Labels
			if req.SkipChunks {
				lset, _ = set.At()
			} else {
				lset, series.Chunks = set.At()

				stats.mergedChunksCount += len(series.Chunks)
				s.metrics.chunkSizeBytes.Observe(float64(chunksSize(series.Chunks)))
			}
			series.Labels = labelpb.ZLabelsFromPromLabels(lset)
			if err = srv.Send(storepb.NewSeriesResponse(&series)); err != nil {
				err = status.Error(codes.Unknown, errors.Wrap(err, "send series response").Error())
				return
			}
		}
		if set.Err() != nil {
			err = status.Error(codes.Unknown, errors.Wrap(set.Err(), "expand series set").Error())
			return
		}
		stats.mergeDuration = time.Since(begin)
		s.metrics.seriesMergeDuration.Observe(stats.mergeDuration.Seconds())

		err = nil
	})

	if s.enableSeriesResponseHints {
		var anyHints *types.Any

		if anyHints, err = types.MarshalAny(resHints); err != nil {
			err = status.Error(codes.Unknown, errors.Wrap(err, "marshal series response hints").Error())
			return
		}

		if err = srv.Send(storepb.NewHintsSeriesResponse(anyHints)); err != nil {
			err = status.Error(codes.Unknown, errors.Wrap(err, "send series response hints").Error())
			return
		}
	}

	return err
}
